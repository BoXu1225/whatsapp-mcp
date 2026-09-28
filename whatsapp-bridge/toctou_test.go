package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setOpenHook(t *testing.T, hook func()) {
	t.Helper()
	sendFileOpenHook = hook
	t.Cleanup(func() { sendFileOpenHook = nil })
}

func TestOpenSendFileReadsCheckedFile(t *testing.T) {
	_, outbox := allowlistFixture(t)
	f, real, err := openSendFile(filepath.Join(outbox, "ok.jpg"), []string{outbox})
	if err != nil {
		t.Fatalf("openSendFile: %v", err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	if string(data) != "fake" || filepath.Base(real) != "ok.jpg" {
		t.Errorf("read %q from %q", data, real)
	}
}

func TestOpenSendFileDetectsSwapAfterCheck(t *testing.T) {
	tests := []struct {
		name string
		path func(outbox string) string
		swap func(t *testing.T, root, outbox string)
	}{
		{
			"file replaced by symlink out",
			func(outbox string) string { return filepath.Join(outbox, "ok.jpg") },
			func(t *testing.T, root, outbox string) {
				real, _ := filepath.EvalSymlinks(filepath.Join(outbox, "ok.jpg"))
				must(t, os.Remove(real))
				must(t, os.Symlink(filepath.Join(root, "secret.txt"), real))
			},
		},
		{
			"parent dir replaced by symlink out",
			func(outbox string) string { return filepath.Join(outbox, "sub", "nested.jpg") },
			func(t *testing.T, root, outbox string) {
				must(t, os.WriteFile(filepath.Join(root, "nested.jpg"), []byte("SECRET"), 0o600))
				realSub, _ := filepath.EvalSymlinks(filepath.Join(outbox, "sub"))
				must(t, os.Rename(realSub, realSub+"-moved"))
				must(t, os.Symlink(root, realSub))
			},
		},
		{
			"file replaced by another file",
			func(outbox string) string { return filepath.Join(outbox, "ok.jpg") },
			func(t *testing.T, root, outbox string) {
				real, _ := filepath.EvalSymlinks(filepath.Join(outbox, "ok.jpg"))
				must(t, os.Rename(filepath.Join(root, "secret.txt"), real))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, outbox := allowlistFixture(t)
			setOpenHook(t, func() { tt.swap(t, root, outbox) })
			f, real, err := openSendFile(tt.path(outbox), []string{outbox})
			if err == nil {
				data, _ := io.ReadAll(f)
				f.Close()
				t.Errorf("openSendFile after swap = %q (read %q), want rejection", real, data)
			}
		})
	}
}

func TestAPISendRejectsSwapAfterCheck(t *testing.T) {
	root, outbox := allowlistFixture(t)
	s, sent := newTestAPIServer(t)
	s.allowedDirs = []string{outbox}
	setOpenHook(t, func() {
		real, _ := filepath.EvalSymlinks(filepath.Join(outbox, "ok.jpg"))
		must(t, os.Remove(real))
		must(t, os.Symlink(filepath.Join(root, "secret.txt"), real))
	})
	body := fmt.Sprintf(`{"recipient":"15550000001","media_path":%q}`, filepath.Join(outbox, "ok.jpg"))
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, validRequest(http.MethodPost, "/api/send", body))
	if rec.Code != http.StatusForbidden || len(*sent) != 0 {
		t.Errorf("-> %d, sent %+v; want 403 and no send", rec.Code, *sent)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
