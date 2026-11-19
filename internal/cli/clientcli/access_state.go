package clientcli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/spf13/cobra"
	pkgClient "github.com/stperic/zzrouter/internal/client/utils"
	"github.com/stperic/zzrouter/pkg/security"
)

// backupsPath is the running coordinator's online backups of its access store.
const backupsPath = "/zzrouter/v1/system/state/backups"

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
	state := &cobra.Command{Use: "state", Short: "Copy this installation's access store"}
	state.AddCommand(export)
	return state
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
	req, err := s.request(ctx, http.MethodPost, backupsPath, []byte("{}"))
	if err != nil {
		return "", err
	}
	response, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("contact the running local installation: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var created struct {
		Data struct{ ID string } `json:"data"`
	}
	err = pkgClient.ReadResponse(response, &created, "backup")
	return created.Data.ID, err
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
