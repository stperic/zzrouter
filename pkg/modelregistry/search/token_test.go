package search

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFindToken(t *testing.T) {
	// saveAndClear saves and unsets all HF token env vars, returning a restore func.
	saveAndClear := func() func() {
		vars := []string{"HF_TOKEN", "HUGGING_FACE_TOKEN"}
		saved := make(map[string]string)
		for _, v := range vars {
			saved[v] = os.Getenv(v)
			_ = os.Unsetenv(v)
		}
		return func() {
			for _, v := range vars {
				if saved[v] != "" {
					_ = os.Setenv(v, saved[v])
				} else {
					_ = os.Unsetenv(v)
				}
			}
		}
	}

	t.Run("env_var_precedence", func(t *testing.T) {
		restore := saveAndClear()
		defer restore()

		testToken := "test-env-token" //nolint:gosec // test fixture, not a credential
		_ = os.Setenv("HUGGING_FACE_TOKEN", testToken)

		if result := FindToken(); result != testToken {
			t.Errorf("FindToken() = %q, want %q", result, testToken)
		}
	})

	t.Run("hf_token_preferred_over_legacy", func(t *testing.T) {
		restore := saveAndClear()
		defer restore()

		_ = os.Setenv("HF_TOKEN", "current")
		_ = os.Setenv("HUGGING_FACE_TOKEN", "older")

		if result := FindToken(); result != "current" {
			t.Errorf("FindToken() = %q, want %q (HF_TOKEN must take precedence)", result, "current")
		}
	})

	t.Run("file_token_fallback", func(t *testing.T) {
		restore := saveAndClear()
		defer restore()

		tempDir := t.TempDir()

		tokenFile := filepath.Join(tempDir, ".cache", "huggingface", "token")
		if err := os.MkdirAll(filepath.Dir(tokenFile), 0755); err != nil {
			t.Fatal(err)
		}
		testToken := "test-file-token"
		if err := os.WriteFile(tokenFile, []byte(testToken), 0600); err != nil {
			t.Fatal(err)
		}

		setTokenTestHome(t, tempDir)

		if result := FindToken(); result != testToken {
			t.Errorf("FindToken() = %q, want %q", result, testToken)
		}
	})

	t.Run("no_token", func(t *testing.T) {
		restore := saveAndClear()
		defer restore()

		setTokenTestHome(t, t.TempDir())

		if result := FindToken(); result != "" {
			t.Errorf("FindToken() = %q, want empty string", result)
		}
	})
}

func setTokenTestHome(t *testing.T, dir string) {
	t.Helper()
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	t.Setenv(key, dir)
}
