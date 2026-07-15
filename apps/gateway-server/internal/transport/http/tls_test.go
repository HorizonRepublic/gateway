package http

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeSelfSignedPair generates an ECDSA self-signed certificate and
// writes the cert and key PEM files to a temp dir, returning their
// paths. Enough to exercise tls.LoadX509KeyPair without a fixture.
func writeSelfSignedPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certOut, err := os.Create(certPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
	require.NoError(t, certOut.Close())

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyOut, err := os.Create(keyPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, keyOut.Close())

	return certPath, keyPath
}

func TestBuildPublicTLSConfig_LoadsPairPinnedTo12(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)

	cfg, err := buildPublicTLSConfig(certPath, keyPath)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion,
		"public TLS must floor at 1.2")
	require.Len(t, cfg.Certificates, 1, "the loaded keypair must be present")
}

func TestBuildPublicTLSConfig_MissingFilesFailClosed(t *testing.T) {
	_, err := buildPublicTLSConfig("/nonexistent/cert.pem", "/nonexistent/key.pem")
	require.Error(t, err)
	assert.ErrorContains(t, err, "load TLS keypair")
}

func TestBuildPublicTLSConfig_MismatchedPairFailClosed(t *testing.T) {
	certA, _ := writeSelfSignedPair(t)
	_, keyB := writeSelfSignedPair(t)

	_, err := buildPublicTLSConfig(certA, keyB)
	require.Error(t, err, "a cert paired with a different key must fail to load")
}
