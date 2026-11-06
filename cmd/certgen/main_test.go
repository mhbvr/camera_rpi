package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerate(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	res, err := generate(dir, []string{"raspberrypi.local", "192.168.1.50", "localhost"}, 825, 3650, now)
	if err != nil || !res.newCA {
		t.Fatalf("first run: %v newCA=%v", err, res.newCA)
	}
	ca := readCert(t, filepath.Join(dir, caCertFile))
	srv := readCert(t, filepath.Join(dir, serverCertFile))
	if !ca.IsCA || !ca.MaxPathLenZero {
		t.Fatal("CA flags")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, h := range []string{"raspberrypi.local", "192.168.1.50", "localhost"} {
		if _, err := srv.Verify(x509.VerifyOptions{DNSName: h, Roots: pool}); err != nil {
			t.Errorf("verify %s: %v", h, err)
		}
	}
	if _, err := srv.Verify(x509.VerifyOptions{DNSName: "other.local", Roots: pool}); err == nil {
		t.Error("certificate should not be valid for other.local")
	}
	// key file permissions and a usable key pair
	for _, f := range []string{caKeyFile, serverKeyFile} {
		st, _ := os.Stat(filepath.Join(dir, f))
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", f, st.Mode().Perm())
		}
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, serverCertFile), filepath.Join(dir, serverKeyFile)); err != nil {
		t.Fatal(err)
	}

	// second run reuses the CA and re-issues the server certificate
	caBefore, _ := os.ReadFile(filepath.Join(dir, caCertFile))
	res, err = generate(dir, []string{"camera.local"}, 30, 3650, now)
	if err != nil || res.newCA {
		t.Fatalf("second run: %v newCA=%v", err, res.newCA)
	}
	caAfter, _ := os.ReadFile(filepath.Join(dir, caCertFile))
	if !bytes.Equal(caBefore, caAfter) {
		t.Fatal("CA was replaced")
	}
	srv2 := readCert(t, filepath.Join(dir, serverCertFile))
	if _, err := srv2.Verify(x509.VerifyOptions{DNSName: "camera.local", Roots: pool}); err != nil {
		t.Fatalf("re-issued cert: %v", err)
	}
}

func TestSplitHosts(t *testing.T) {
	got := splitHosts(" a.local, 10.0.0.1 ,,b ")
	if len(got) != 3 || got[0] != "a.local" || got[1] != "10.0.0.1" || got[2] != "b" {
		t.Fatalf("%q", got)
	}
	if _, err := generate(t.TempDir(), nil, 1, 1, time.Now()); err == nil {
		t.Fatal("expected error for no hosts")
	}
}
