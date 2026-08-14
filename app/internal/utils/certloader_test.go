package utils

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testCAFile   = "./testcerts/ca"
	testCertFile = "./testcerts/cert"
	testKeyFile  = "./testcerts/key"
)

func TestCertificateLoaderPathError(t *testing.T) {
	assert.NoError(t, os.RemoveAll(testCertFile))
	assert.NoError(t, os.RemoveAll(testKeyFile))
	loader := LocalCertificateLoader{
		CertFile: testCertFile,
		KeyFile:  testKeyFile,
		SNIGuard: SNIGuardStrict,
	}
	err := loader.InitializeCache()
	var pathErr *os.PathError
	assert.ErrorAs(t, err, &pathErr)
}

func TestCertificateLoaderFullChain(t *testing.T) {
	require.NoError(t, generateTestCertificate([]string{"example.com"}, "fullchain"))

	loader := LocalCertificateLoader{
		CertFile: testCertFile,
		KeyFile:  testKeyFile,
		SNIGuard: SNIGuardStrict,
	}
	require.NoError(t, loader.InitializeCache())
	testListen := startTestTLSServer(t, &loader)

	assert.Error(t, runTestTLSClient(testListen, "unmatched-sni.example.com"))
	assert.Error(t, runTestTLSClient(testListen, ""))
	assert.NoError(t, runTestTLSClient(testListen, "example.com"))
}

func TestCertificateLoaderNoSAN(t *testing.T) {
	require.NoError(t, generateTestCertificate(nil, "selfsign"))

	loader := LocalCertificateLoader{
		CertFile: testCertFile,
		KeyFile:  testKeyFile,
		SNIGuard: SNIGuardDNSSAN,
	}
	require.NoError(t, loader.InitializeCache())
	testListen := startTestTLSServer(t, &loader)

	assert.NoError(t, runTestTLSClient(testListen, ""))
}

func TestCertificateLoaderReplaceCertificate(t *testing.T) {
	require.NoError(t, generateTestCertificate([]string{"example.com"}, "fullchain"))

	loader := LocalCertificateLoader{
		CertFile: testCertFile,
		KeyFile:  testKeyFile,
		SNIGuard: SNIGuardStrict,
	}
	require.NoError(t, loader.InitializeCache())
	testListen := startTestTLSServer(t, &loader)

	assert.NoError(t, runTestTLSClient(testListen, "example.com"))
	assert.Error(t, runTestTLSClient(testListen, "2.example.com"))

	require.NoError(t, generateTestCertificate([]string{"2.example.com"}, "fullchain"))

	assert.Error(t, runTestTLSClient(testListen, "example.com"))
	assert.NoError(t, runTestTLSClient(testListen, "2.example.com"))
}

func startTestTLSServer(t *testing.T, loader *LocalCertificateLoader) string {
	t.Helper()

	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	listener := tls.NewListener(rawListener, &tls.Config{
		GetCertificate: loader.GetCertificate,
	})
	server := &http.Server{ReadHeaderTimeout: time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		require.NoError(t, server.Close())
		select {
		case err := <-serveDone:
			require.ErrorIs(t, err, http.ErrServerClosed)
		case <-time.After(5 * time.Second):
			t.Error("TLS test server did not stop after Close")
		}
	})
	return listener.Addr().String()
}

func generateTestCertificate(dnssan []string, certType string) error {
	args := []string{
		"certloader_test_gencert.py",
		"--ca", testCAFile,
		"--cert", testCertFile,
		"--key", testKeyFile,
		"--type", certType,
	}
	if len(dnssan) > 0 {
		args = append(args, "--dnssan", strings.Join(dnssan, ","))
	}
	cmd := exec.Command("python", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to generate test certificate: %s", out)
		return err
	}
	return nil
}

func runTestTLSClient(server, sni string) error {
	args := []string{
		"certloader_test_tlsclient.py",
		"--server", server,
		"--ca", testCAFile,
	}
	if sni != "" {
		args = append(args, "--sni", sni)
	}
	cmd := exec.Command("python", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Failed to run test TLS client: %s", out)
		return err
	}
	return nil
}
