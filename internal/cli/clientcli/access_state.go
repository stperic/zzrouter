package clientcli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/security"
)

const (
	// backupsPath is the running coordinator's online backups of its access store.
	backupsPath = "/zzrouter/v1/system/state/backups"
	// resetPath resets its access store; resetPath + "/preview" previews that.
	resetPath = "/zzrouter/v1/system/state/reset"
)

func newAccessStateCmd() *cobra.Command {
	var dir, id, output string
	export := &cobra.Command{Use: "export", Short: "Copy an online backup of the access store into a new file", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := exportBackup(ctx, dir, id, output); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s. It holds credential hashes: keep it in protected storage.\nCheck it before you rely on it: zzrouter-node recovery check %s\n", output, output)
		return err
	}}
	export.Flags().StringVar(&dir, "dir", "", "Installed configuration directory")
	export.Flags().StringVar(&id, "backup-id", "", "Retained backup to copy; without it, a new backup is taken")
	export.Flags().StringVar(&output, "output", "", "New file outside the installation")
	_ = export.MarkFlagRequired("output")
	var yes bool
	reset := &cobra.Command{Use: "reset", Short: "Remove every app key, team and person but the owner, after a backup", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return resetAccessState(ctx, cmd.OutOrStdout(), dir, id, yes)
	}}
	reset.Flags().StringVar(&dir, "dir", "", "Installed configuration directory")
	reset.Flags().StringVar(&id, "backup-id", "", "Retained backup of the current records; without it, a new backup is taken")
	reset.Flags().BoolVar(&yes, "yes", false, "Reset without asking for confirmation")
	state := &cobra.Command{Use: "state", Short: "Copy or reset this installation's access store"}
	state.AddCommand(export, reset)
	return state
}

// resetAccessState backs up the access store unless id names a backup,
// previews the reset against it, prints what goes and what stays, asks unless
// yes, then resets and prints what the reset did and how to undo it.
func resetAccessState(ctx context.Context, out io.Writer, dir, id string, yes bool) error {
	session, err := openLocal(ctx, dir)
	if err != nil {
		return err
	}
	defer session.client.CloseIdleConnections()
	if id == "" {
		if id, err = session.createBackup(ctx); err != nil {
			return err
		}
	}
	var preview struct {
		Data struct {
			Revision uint64 `json:"revision"`
			Removes  struct {
				AppKeys int `json:"app_keys"`
				Teams   int `json:"teams"`
				People  int `json:"people"`
				Grants  int `json:"grants"`
			} `json:"removes"`
			Keeps *struct {
				OwnerUsername string `json:"owner_username"`
				TeamName      string `json:"team_name"`
			} `json:"keeps"`
		} `json:"data"`
	}
	body, _ := json.Marshal(map[string]string{"backup_id": id}) // a map of strings always encodes
	if err := session.post(ctx, resetPath+"/preview", body, "", &preview, "reset preview"); err != nil {
		return err
	}
	p := preview.Data
	_, _ = fmt.Fprintf(out, "Backup %s holds the access records at revision %d.\nThe reset removes app keys: %d, teams: %d, people: %d, invitation and sign-in grants: %d, and the onboarding progress.\n",
		id, p.Revision, p.Removes.AppKeys, p.Removes.Teams, p.Removes.People, p.Removes.Grants)
	if p.Keeps == nil {
		_, _ = fmt.Fprintln(out, "No owner has been claimed: nothing stays, and owner setup stays open.")
	} else {
		_, _ = fmt.Fprintf(out, "It keeps the owner %s, who must sign in again, and their team %q with its policy and spend.\n", p.Keeps.OwnerUsername, p.Keeps.TeamName)
	}
	if !yes && !confirmed(ctx, func() bool { return confirmYN("Reset the access store? [y/N]: ") }) {
		return errors.New("not confirmed: nothing was reset")
	}
	var result struct {
		Data struct {
			OperationID string `json:"operation_id"`
			NewRevision uint64 `json:"new_revision"`
		} `json:"data"`
	}
	// net/http resends a request carrying an Idempotency-Key once when a reused
	// connection drops it, and the node replays a reset it already did.
	if err = session.post(ctx, resetPath, body, uuid.NewString(), &result, "reset"); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Reset %s committed at revision %d.\nThe owner signs in again with their password (zzrouter access recover-owner if it is lost), then creates new app keys.\n"+
		"To undo it while backup %s is retained: zzrouter access state export --backup-id %s --output FILE, stop the service, zzrouter-node recovery restore FILE --to STAGING --dir %[5]s, then zzrouter-node recovery activate STAGING --dir %[5]s, after a --dry-run. The spend of removed keys and teams does not come back.\n",
		result.Data.OperationID, result.Data.NewRevision, id, id, session.dir)
	return err
}

// confirmed asks with ask, answering no as soon as ctx ends, so an interrupt
// at the prompt stops the command instead of waiting for Enter. The read the
// prompt left blocked ends with the process.
func confirmed(ctx context.Context, ask func() bool) bool {
	answer := make(chan bool, 1)
	go func() { answer <- ask() }()
	select {
	case ok := <-answer:
		return ok
	case <-ctx.Done():
		return false
	}
}

// exportBackup copies a retained backup, a new one when id is empty, from the
// installation's running node into output, a new file outside the
// installation, and checks its digest. A failure, an interrupt included,
// leaves no file.
func exportBackup(ctx context.Context, dir, id, output string) (err error) {
	session, err := openLocal(ctx, dir)
	if err != nil {
		return err
	}
	defer session.client.CloseIdleConnections()
	if err := newOutsideInstallation(session.dir, output); err != nil {
		return err
	}
	if id == "" {
		if id, err = session.createBackup(ctx); err != nil {
			return err
		}
	}
	req, err := session.request(ctx, http.MethodGet, backupsPath+"/"+url.PathEscape(id)+"/content", nil)
	if err != nil {
		return err
	}
	response, err := session.client.Do(req)
	if err != nil {
		return fmt.Errorf("contact the running local installation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		err = pkgClient.ReadResponse(response, nil, "export")
		var refused *pkgClient.APIError
		if errors.As(err, &refused) && refused.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w\nWithout --backup-id, the command takes a new backup and exports it", err)
		}
		return cmp.Or(err, fmt.Errorf("export: unexpected status %d", response.StatusCode))
	}
	partial := output + ".partial"
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(partial)
		}
	}()
	sum := sha256.New()
	err = security.RestrictFilePermissions(partial)
	if err == nil {
		_, err = io.Copy(io.MultiWriter(f, sum), response.Body)
		if err == nil && response.Header.Get("Repr-Digest") != "sha-256=:"+base64.StdEncoding.EncodeToString(sum.Sum(nil))+":" {
			err = errors.New("the copy does not match the digest the node sent")
		}
	}
	if err = errors.Join(err, f.Sync(), f.Close()); err == nil {
		err = os.Rename(partial, output)
	}
	return err
}

// createBackup takes a new online backup and returns its ID.
func (s *localSession) createBackup(ctx context.Context) (string, error) {
	var created struct {
		Data struct{ ID string } `json:"data"`
	}
	err := s.post(ctx, backupsPath, []byte("{}"), "", &created, "backup")
	return created.Data.ID, err
}

// post sends body to path, under the Idempotency-Key key unless it is empty,
// and reads the answer into target.
func (s *localSession) post(ctx context.Context, path string, body []byte, key string, target any, operation string) error {
	req, err := s.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("contact the running local installation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	return pkgClient.ReadResponse(response, target, operation)
}

// newOutsideInstallation refuses an output that exists or whose directory is
// the installation's or lies beneath it. Ancestors are compared by file
// identity, walked through ".." from the directory as given, so neither a
// link nor the case of a spelling can hide the installation.
func newOutsideInstallation(dir, output string) error {
	if _, err := os.Lstat(output); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s exists: choose a new file", output)
	}
	installation, err := os.Stat(dir)
	if err != nil {
		return err
	}
	// The trailing dot keeps a drive-relative Windows output (C:name) in its
	// own directory instead of jumping to the drive root.
	ancestor := strings.TrimSuffix(output, filepath.Base(output)) + "."
	for previous := os.FileInfo(nil); ; ancestor += string(filepath.Separator) + ".." {
		info, err := os.Stat(ancestor)
		switch {
		case err != nil:
			return err
		case os.SameFile(info, installation):
			return fmt.Errorf("%s is inside the installation: write the copy elsewhere", output)
		case previous != nil && os.SameFile(info, previous): // the root
			return nil
		}
		previous = info
	}
}
