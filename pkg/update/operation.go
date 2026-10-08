package update

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/google/uuid"
)

func (s *Scheduler) recordOperation(id string, release *ReleaseInfo) (string, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if id == "" {
		id = uuid.NewString()
	}
	run := RunStatus{PID: os.Getpid(), JobID: id, Action: ActionApply, ToVersion: release.Version.String(), State: StateChecking, StartedAt: s.clock.Now().UTC()}
	if err := writeJSONAtomic(s.operationPath, &run, 0600); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.status.PendingRelease, s.status.State, s.status.Error = release, StateChecking, ""
	s.mu.Unlock()
	return id, nil
}

func (s *Scheduler) markOperationRestarting() error {
	if s.delegate != nil {
		return nil
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	var run RunStatus
	if err := readJSON(s.operationPath, &run); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if run.Finished() || run.PID != os.Getpid() {
		return nil
	}
	run.State = StateRestarting
	return writeJSONAtomic(s.operationPath, &run, 0600)
}

func (s *Scheduler) finishOperation(cause error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	var run RunStatus
	if err := readJSON(s.operationPath, &run); err != nil {
		return
	}
	finished := s.clock.Now().UTC()
	run.FinishedAt, run.State, run.Error = &finished, StateFailed, cause.Error()
	if err := writeJSONAtomic(s.operationPath, &run, 0600); err != nil {
		slog.Error("could not record failed update operation", "err", err)
		s.setError(fmt.Errorf("record failed operation: %w", err))
	}
}
