// Command gencert generates a self-signed Ed25519 server certificate and
// prints its SPKI sha256 fingerprint (used by builder.py as the pin).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"
)

func main() {
	out := flag.String("out", "server.crt", "cert output path")
	keyPath := flag.String("key", "server.key", "key output path")
	days := flag.Int("days", 3650, "validity days")
	flag.Parse()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err)

	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "she11 server", Organization: []string{"she11"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Duration(*days) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, // self-signed anchor
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost", "she11"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, priv)
	must(err)

	// SPKI fingerprint = sha256 of SubjectPublicKeyInfo.
	spki, err := x509.MarshalPKIXPublicKey(pub)
	must(err)
	fp := sha256.Sum256(spki)

	must(os.WriteFile(*out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644))
	keyDer, err := x509.MarshalPKCS8PrivateKey(priv)
	must(err)
	must(os.WriteFile(*keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer}), 0600))

	fmt.Printf("cert:   %s\nkey:    %s\nfingerprint(SPKI sha256, hex): %s\n", *out, *keyPath, hex.EncodeToString(fp[:]))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "gencert:", err)
		os.Exit(1)
	}
}
