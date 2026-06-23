package router

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanDownloadFilePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "relative file", in: "foo/bar.txt", want: "/foo/bar.txt", ok: true},
		{name: "absolute file", in: "/foo/bar.txt", want: "/foo/bar.txt", ok: true},
		{name: "backslashes", in: `foo\bar.txt`, want: "/foo/bar.txt", ok: true},
		{name: "empty", in: "", ok: false},
		{name: "root", in: "/", ok: false},
		{name: "traversal", in: "/foo/../bar.txt", ok: false},
		{name: "nul byte", in: "foo\x00bar.txt", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cleanDownloadFilePath(tt.in)
			if ok != tt.ok {
				t.Fatalf("expected ok %t, got %t", tt.ok, ok)
			}
			if got != tt.want {
				t.Fatalf("expected path %q, got %q", tt.want, got)
			}
		})
	}
}

func TestCleanUploadFilename(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "simple", in: "world.zip", want: "world.zip", ok: true},
		{name: "spaces", in: "my world.zip", want: "my world.zip", ok: true},
		{name: "empty", in: "", ok: false},
		{name: "dot", in: ".", ok: false},
		{name: "dot dot", in: "..", ok: false},
		{name: "forward slash", in: "../world.zip", ok: false},
		{name: "nested path", in: "saves/world.zip", ok: false},
		{name: "backslash", in: `saves\world.zip`, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cleanUploadFilename(tt.in)
			if ok != tt.ok {
				t.Fatalf("expected ok %t, got %t", tt.ok, ok)
			}
			if got != tt.want {
				t.Fatalf("expected filename %q, got %q", tt.want, got)
			}
		})
	}
}

func TestFixedDownloadReader(t *testing.T) {
	t.Run("reads unchanged file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "download.txt")
		if err := os.WriteFile(path, []byte("download contents"), 0o644); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(got))
		n, err := newFixedDownloadReader(f, info.Size()).Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if string(buf[:n]) != string(got) {
			t.Fatalf("expected %q, got %q", string(got), string(buf[:n]))
		}
	})

	t.Run("caps file growth to original size", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "download.txt")
		if err := os.WriteFile(path, []byte("download"), 0o644); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		reader := newFixedDownloadReader(f, info.Size())

		if err := os.WriteFile(path, []byte("download with extra bytes"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "download" {
			t.Fatalf("expected %q, got %q", "download", string(got))
		}
	})

	t.Run("pads when file shrinks before fixed size is read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "download.txt")
		if err := os.WriteFile(path, []byte("download"), 0o644); err != nil {
			t.Fatal(err)
		}

		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		reader := newFixedDownloadReader(f, info.Size())

		if err := os.WriteFile(path, []byte("down"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]byte("down"), bytes.Repeat([]byte{0}, 4)...)
		if !bytes.Equal(got, want) {
			t.Fatalf("expected %q, got %q", want, got)
		}
	})
}
