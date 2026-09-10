package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEnvFileMissing(t *testing.T) {
	values, err := readEnvFile(filepath.Join(t.TempDir(), "нет-такого"))
	if err != nil {
		t.Fatalf("отсутствующий файл не должен быть ошибкой: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("ожидалась пустая карта, получено %v", values)
	}
}

func TestReadEnvFileParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.env")
	content := "" +
		"# комментарий\n" +
		"\n" +
		"ADMIN_TOKEN=abc\n" +
		"  JWT_SIGNING_KEY = xyz  \n" +
		// Значение само содержит '=' — разбор обязан делиться по первому.
		"DATABASE_URL=postgres://u:p@h:5432/db?sslmode=require&x=1\n" +
		"мусор-без-знака-равенства\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	values, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	want := map[string]string{
		"ADMIN_TOKEN":     "abc",
		"JWT_SIGNING_KEY": "xyz",
		"DATABASE_URL":    "postgres://u:p@h:5432/db?sslmode=require&x=1",
	}
	if len(values) != len(want) {
		t.Fatalf("получено %v, ожидалось %v", values, want)
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, ожидалось %q", k, values[k], v)
		}
	}
}

func TestWriteAtomicPermissionsAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.env")
	values := map[string]string{
		"DATABASE_URL":    "postgres://u:p@h:5432/db?sslmode=require",
		"JWT_SIGNING_KEY": "k",
		"ADMIN_TOKEN":     "t",
	}
	if err := writeAtomic(path, renderEnv(values)); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Файл с секретами не должен быть доступен на чтение никому, кроме владельца.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("права %o, ожидалось 600", perm)
	}

	back, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	for k, v := range values {
		if back[k] != v {
			t.Errorf("после записи и чтения %s = %q, ожидалось %q", k, back[k], v)
		}
	}
}

func TestWriteAtomicOverwritesWithoutLeavingTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.env")
	for _, content := range []string{"A=1\n", "A=2\nB=3\n"} {
		if err := writeAtomic(path, content); err != nil {
			t.Fatalf("writeAtomic: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".broker-env-") {
			t.Errorf("остался временный файл %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("в каталоге %d файлов, ожидался один", len(entries))
	}
}

// TestRenderEnvIsStable: файл дописывается повторными запусками, и
// переупорядочивание строк давало бы бессмысленный diff.
func TestRenderEnvIsStable(t *testing.T) {
	values := map[string]string{"B": "2", "A": "1", "C": "3"}
	first := renderEnv(values)
	if first != renderEnv(values) {
		t.Fatal("вывод renderEnv нестабилен между вызовами")
	}
	body := first[strings.Index(first, "A="):]
	if !strings.HasPrefix(body, "A=1\nB=2\nC=3\n") {
		t.Errorf("переменные не отсортированы: %q", body)
	}
}
