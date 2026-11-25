package obfs

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/blake2b"
)

func BenchmarkSalamanderObfuscator_Obfuscate(b *testing.B) {
	o, _ := newSalamanderObfuscator([]byte("average_password"))
	in := make([]byte, 1200)
	_, _ = rand.Read(in)
	out := make([]byte, 2048)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o.Obfuscate(in, out)
	}
}

func BenchmarkSalamanderObfuscator_Deobfuscate(b *testing.B) {
	o, _ := newSalamanderObfuscator([]byte("average_password"))
	in := make([]byte, 1200)
	_, _ = rand.Read(in)
	out := make([]byte, 2048)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o.Deobfuscate(in, out)
	}
}

func TestSalamanderObfuscator(t *testing.T) {
	o, _ := newSalamanderObfuscator([]byte("average_password"))
	in := make([]byte, 1200)
	oOut := make([]byte, 2048)
	dOut := make([]byte, 2048)
	for i := 0; i < 1000; i++ {
		_, _ = rand.Read(in)
		n := o.Obfuscate(in, oOut)
		assert.Equal(t, len(in)+smSaltLen, n)
		n = o.Deobfuscate(oOut[:n], dOut)
		assert.Equal(t, len(in), n)
		assert.Equal(t, in, dOut[:n])
	}
}

// Compare with the byte-wise wire definition independently of the optimized
// send and receive paths, especially where the digest repeats.
func TestSalamanderWireCompatibility(t *testing.T) {
	password := []byte("test password")
	o, err := newSalamanderObfuscator(password)
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, 7, 8, 31, 32, 33, 63, 64, 65, 1192, 1200, udpBufferSize - smSaltLen} {
		plain := make([]byte, size)
		for i := range plain {
			plain[i] = byte(i)
		}
		wire := bytes.Repeat([]byte{0xaa}, size+smSaltLen+1)
		n := o.Obfuscate(plain, wire)
		if n != size+smSaltLen || wire[n] != 0xaa {
			t.Fatalf("size %d: unexpected output length or overwritten sentinel", size)
		}
		key := blake2b.Sum256(append(bytes.Clone(password), wire[:smSaltLen]...))
		for i, b := range plain {
			if wire[smSaltLen+i] != b^key[i%len(key)] {
				t.Fatalf("size %d: wire byte %d differs", size, i)
			}
		}
		decoded := bytes.Repeat([]byte{0xaa}, size+1)
		if o.Deobfuscate(wire[:n], decoded) != size || !bytes.Equal(decoded[:size], plain) || decoded[size] != 0xaa {
			t.Fatalf("size %d: unexpected decode result", size)
		}
	}
}
