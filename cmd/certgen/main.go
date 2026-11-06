// Command certgen creates a private certificate authority (CA) and a server
// certificate signed by it, so camweb can serve HTTPS on the LAN (browsers
// only allow WebCodecs on HTTPS or localhost).
//
//	go run ./cmd/certgen -dir certs -hosts raspberrypi.local,192.168.1.50
//
// The CA (ca.crt, ca.key) is created once and reused, so it has to be
// installed in browsers/OSes only once; the server certificate is
// re-issued on every run (for example after adding a host name or IP).
// Keep ca.key private: anyone holding it can issue certificates your
// browser will trust.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Output file names inside -dir.
const (
	caCertFile     = "ca.crt"
	caKeyFile      = "ca.key"
	serverCertFile = "server.crt"
	serverKeyFile  = "server.key"
)

func main() {
	dir := flag.String("dir", "certs", "output directory")
	hosts := flag.String("hosts", "localhost,127.0.0.1", "comma-separated DNS names and IP addresses for the server certificate")
	days := flag.Int("days", 825, "server certificate validity in days (Apple devices reject more than 825)")
	caDays := flag.Int("ca-days", 3650, "CA validity in days (only used when a new CA is created)")
	flag.Parse()

	res, err := generate(*dir, splitHosts(*hosts), *days, *caDays, time.Now())
	if err != nil {
		log.Fatal(err)
	}
	if res.newCA {
		fmt.Printf("created CA            %s (install this in your browser/OS as a trusted CA)\n", filepath.Join(*dir, caCertFile))
	} else {
		fmt.Printf("reused CA             %s\n", filepath.Join(*dir, caCertFile))
	}
	fmt.Printf("server certificate    %s\n", filepath.Join(*dir, serverCertFile))
	fmt.Printf("server key            %s\n", filepath.Join(*dir, serverKeyFile))
	fmt.Printf("valid for             %s (until %s)\n", strings.Join(res.hosts, ", "), res.notAfter.Format("2006-01-02"))
}

func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

type result struct {
	newCA    bool
	hosts    []string
	notAfter time.Time
}

// generate loads (or creates) the CA in dir and issues a fresh server
// certificate for hosts.
func generate(dir string, hosts []string, days, caDays int, now time.Time) (result, error) {
	var res result
	if len(hosts) == 0 {
		return res, errors.New("no hosts given")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	caCert, caKey, err := loadCA(dir)
	if errors.Is(err, fs.ErrNotExist) {
		caCert, caKey, err = createCA(dir, caDays, now)
		res.newCA = true
	}
	if err != nil {
		return res, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return res, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: hosts[0], Organization: []string{"camera_rpi"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return res, err
	}
	if err := writePEM(filepath.Join(dir, serverCertFile), "CERTIFICATE", der, 0o644); err != nil {
		return res, err
	}
	if err := writeKey(filepath.Join(dir, serverKeyFile), key); err != nil {
		return res, err
	}
	res.hosts, res.notAfter = hosts, tmpl.NotAfter
	return res, nil
}

func createCA(dir string, days int, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "camera_rpi local CA (" + host + ")", Organization: []string{"camera_rpi"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(0, 0, days),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true, // may sign server certificates only, not other CAs
		SubjectKeyId:          keyID(&key.PublicKey),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := writeKey(filepath.Join(dir, caKeyFile), key); err != nil {
		return nil, nil, err
	}
	if err := writePEM(filepath.Join(dir, caCertFile), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("%s exists but its key is missing: %w", caCertFile, err)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("cannot decode CA PEM files")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}

func keyID(pub *ecdsa.PublicKey) []byte {
	b, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha1.Sum(b)
	return sum[:]
}

func writeKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(path, "EC PRIVATE KEY", der, 0o600)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, mode) // also for files that already existed
}
