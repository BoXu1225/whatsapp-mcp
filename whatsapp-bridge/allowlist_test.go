package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allowlistFixture builds:
//
//	root/secret.txt
//	root/outbox/ok.jpg
//	root/outbox/sub/nested.jpg
//	root/outbox/escape.jpg   -> root/secret.txt   (file symlink out)
//	root/outbox/rootlink     -> root              (dir symlink out)
//	root/outbox/alias.jpg    -> root/outbox/ok.jpg (symlink that stays in)
//	root/outbox-evil/a.jpg                        (shares the outbox prefix)
//	root/extra/doc.pdf
//
// root comes from t.TempDir, which on macOS sits behind the /var symlink, so
// the allowed dirs are deliberately passed unresolved.
func allowlistFixture(t *testing.T) (root, outbox string) {
	t.Helper()
	root = t.TempDir()
	outbox = filepath.Join(root, "outbox")
	mustMkdir(t, filepath.Join(outbox, "sub"))
	mustMkdir(t, filepath.Join(root, "outbox-evil"))
	mustMkdir(t, filepath.Join(root, "extra"))
	mustWrite(t, filepath.Join(root, "secret.txt"))
	mustWrite(t, filepath.Join(outbox, "ok.jpg"))
	mustWrite(t, filepath.Join(outbox, "sub", "nested.jpg"))
	mustWrite(t, filepath.Join(root, "outbox-evil", "a.jpg"))
	mustWrite(t, filepath.Join(root, "extra", "doc.pdf"))
	mustSymlink(t, filepath.Join(root, "secret.txt"), filepath.Join(outbox, "escape.jpg"))
	mustSymlink(t, root, filepath.Join(outbox, "rootlink"))
	mustSymlink(t, filepath.Join(outbox, "ok.jpg"), filepath.Join(outbox, "alias.jpg"))
	return root, outbox
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestResolveSendPath(t *testing.T) {
	root, outbox := allowlistFixture(t)
	realOutbox, err := filepath.EvalSymlinks(outbox)
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{outbox}

	tests := []struct {
		name    string
		path    string
		allowed []string
		want    string // resolved path when accepted; "" means rejected
	}{
		{"file in outbox", filepath.Join(outbox, "ok.jpg"), allowed, filepath.Join(realOutbox, "ok.jpg")},
		{"nested file in outbox", filepath.Join(outbox, "sub", "nested.jpg"), allowed, filepath.Join(realOutbox, "sub", "nested.jpg")},
		{"symlink staying inside", filepath.Join(outbox, "alias.jpg"), allowed, filepath.Join(realOutbox, "ok.jpg")},
		{"dotdot staying inside", filepath.Join(outbox, "sub", "..", "ok.jpg"), allowed, filepath.Join(realOutbox, "ok.jpg")},
		{"absolute path outside", filepath.Join(root, "secret.txt"), allowed, ""},
		{"system file", "/etc/hosts", allowed, ""},
		{"dotdot traversal", outbox + "/../secret.txt", allowed, ""},
		{"dotdot traversal unclean", outbox + string(filepath.Separator) + ".." + string(filepath.Separator) + "secret.txt", allowed, ""},
		{"file symlink escaping", filepath.Join(outbox, "escape.jpg"), allowed, ""},
		{"dir symlink escaping", filepath.Join(outbox, "rootlink", "secret.txt"), allowed, ""},
		{"sibling sharing prefix", filepath.Join(root, "outbox-evil", "a.jpg"), allowed, ""},
		{"the outbox dir itself", outbox, allowed, ""},
		{"missing file in outbox", filepath.Join(outbox, "nope.jpg"), allowed, ""},
		{"empty path", "", allowed, ""},
		{"relative path", "secret.txt", allowed, ""},
		{"no allowed dirs", filepath.Join(outbox, "ok.jpg"), nil, ""},
		{"extra allowed dir", filepath.Join(root, "extra", "doc.pdf"), []string{outbox, filepath.Join(root, "extra")}, "extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSendPath(tt.path, tt.allowed)
			if tt.want == "" {
				if err == nil {
					t.Errorf("resolveSendPath(%q) = %q, want rejection", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSendPath(%q) rejected: %v", tt.path, err)
			}
			if tt.want == "extra" {
				if !strings.HasSuffix(got, filepath.Join("extra", "doc.pdf")) || !filepath.IsAbs(got) {
					t.Errorf("got %q, want resolved extra/doc.pdf", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("resolveSendPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestSendAllowedDirs(t *testing.T) {
	store := t.TempDir()
	extraA := t.TempDir()
	extraB := t.TempDir()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv(sendAllowedDirsEnv, strings.Join([]string{extraA, "", missing, extraB}, string(os.PathListSeparator)))

	dirs, err := sendAllowedDirs(store)
	if err != nil {
		t.Fatalf("sendAllowedDirs: %v", err)
	}

	outbox := filepath.Join(store, outboxDirName)
	info, err := os.Stat(outbox)
	if err != nil {
		t.Fatalf("outbox not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("outbox mode = %o, want 700", perm)
	}

	want := []string{}
	for _, d := range []string{outbox, extraA, extraB} {
		r, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, r)
	}
	if fmt.Sprint(dirs) != fmt.Sprint(want) {
		t.Errorf("dirs = %v, want %v (missing and empty entries dropped, all resolved)", dirs, want)
	}
}

func TestSendAllowedDirsDefaultIsOutboxOnly(t *testing.T) {
	t.Setenv(sendAllowedDirsEnv, "")
	store := t.TempDir()
	dirs, err := sendAllowedDirs(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || filepath.Base(dirs[0]) != outboxDirName {
		t.Errorf("dirs = %v, want only the outbox", dirs)
	}
}

func TestAPISendRejectsPathOutsideAllowlist(t *testing.T) {
	root, outbox := allowlistFixture(t)
	realOutbox, _ := filepath.EvalSymlinks(outbox)

	tests := []struct {
		name     string
		path     string
		wantCode int
		wantSent string
	}{
		{"outside", filepath.Join(root, "secret.txt"), http.StatusForbidden, ""},
		{"traversal", outbox + "/../secret.txt", http.StatusForbidden, ""},
		{"symlink escape", filepath.Join(outbox, "escape.jpg"), http.StatusForbidden, ""},
		{"inside", filepath.Join(outbox, "ok.jpg"), http.StatusOK, filepath.Join(realOutbox, "ok.jpg")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sent := newTestAPIServer(t)
			s.allowedDirs = []string{outbox}
			body := fmt.Sprintf(`{"recipient":"15550000001","media_path":%q}`, tt.path)
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/send", body))
			if rec.Code != tt.wantCode {
				t.Errorf("-> %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantSent == "" {
				if len(*sent) != 0 {
					t.Errorf("sender called with %+v, want no send", *sent)
				}
				return
			}
			if len(*sent) != 1 || (*sent)[0].mediaPath != tt.wantSent {
				t.Errorf("sent %+v, want media path %q", *sent, tt.wantSent)
			}
		})
	}
}
