package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"math/rand"
	"testing"
)

// refMeta is a deliberately naive port of mg-verify's algorithm (whole-buffer, one
// block at a time), used as the oracle for the streaming implementation.
func refMeta(key [16]byte, nonce [8]byte, data []byte) [16]byte {
	block, _ := aes.NewCipher(key[:])
	var meta [16]byte
	off, idx := 0, 0
	for off < len(data) {
		size := (min(idx, 7) + 1) * macChunkBase
		size = min(size, len(data)-off)
		var mac [16]byte
		copy(mac[:8], nonce[:])
		copy(mac[8:], nonce[:])
		for p := 0; p < size; p += 16 {
			var blk [16]byte
			copy(blk[:], data[off+p:off+min(p+16, size)])
			for i := range mac {
				mac[i] ^= blk[i]
			}
			block.Encrypt(mac[:], mac[:])
		}
		for i := range meta {
			meta[i] ^= mac[i]
		}
		block.Encrypt(meta[:], meta[:])
		off += size
		idx++
	}
	return meta
}

func TestMACBoundaries(t *testing.T) {
	// The first boundaries are the running sums of 1..8 × 128 KiB, then +1 MiB.
	want := []int64{0}
	var pos int64
	for i := int64(1); i <= 8; i++ {
		pos += i * macChunkBase
		want = append(want, pos)
	}
	for i := 0; i < 5; i++ {
		pos += macSteadyStep
		want = append(want, pos)
	}
	for i := 0; i+1 < len(want); i++ {
		if got := macChunkEnd(want[i]); got != want[i+1] {
			t.Fatalf("macChunkEnd(%d) = %d, want %d", want[i], got, want[i+1])
		}
		if !isMACBoundary(want[i]) {
			t.Fatalf("%d should be a boundary", want[i])
		}
		if want[i]+1 < want[i+1] && isMACBoundary(want[i]+1) {
			t.Fatalf("%d should not be a boundary", want[i]+1)
		}
		mid := (want[i] + want[i+1]) / 2
		if got := macBoundaryAtOrAfter(mid); got != want[i+1] {
			t.Fatalf("macBoundaryAtOrAfter(%d) = %d, want %d", mid, got, want[i+1])
		}
		if got := macBoundaryFloor(mid, 1<<40); got != want[i] {
			t.Fatalf("macBoundaryFloor(%d) = %d, want %d", mid, got, want[i])
		}
	}
	if macSteadyStart != 4608*1024 {
		t.Fatalf("steady start %d", macSteadyStart)
	}
}

func TestMACStreamMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var key [16]byte
	var nonce [8]byte
	rng.Read(key[:])
	rng.Read(nonce[:])

	sizes := []int{1, 15, 16, 17, macChunkBase - 1, macChunkBase, macChunkBase + 1,
		macSteadyStart - 3, macSteadyStart, macSteadyStart + 5, 6<<20 + 7}
	for _, size := range sizes {
		data := make([]byte, size)
		rng.Read(data)
		want := refMeta(key, nonce, data)

		// Feed in random-sized pieces, as network reads arrive.
		m := newMACStream(key, nonce, 0, int64(size))
		f := newMetaFold(key, [16]byte{})
		for p := 0; p < size; {
			n := min(1+rng.Intn(300000), size-p)
			for _, c := range m.Write(data[p : p+n]) {
				f.add(c)
			}
			p += n
		}
		if f.state != want {
			t.Fatalf("size %d: streaming meta-MAC %x, reference %x", size, f.state, want)
		}
	}
}

// Starting a MAC stream mid-file at a boundary, as resumed chunks do, must produce
// exactly the chunk MACs a stream from zero produces past that point.
func TestMACStreamFromOffset(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	var key [16]byte
	var nonce [8]byte
	rng.Read(key[:])
	rng.Read(nonce[:])
	data := make([]byte, 7<<20+123)
	rng.Read(data)

	full := newMACStream(key, nonce, 0, int64(len(data))).Write(data)
	start := macBoundaryAtOrAfter(5 << 20)
	skipped := 0
	for b := int64(0); b < start; b = macChunkEnd(b) {
		skipped++
	}
	part := newMACStream(key, nonce, start, int64(len(data))).Write(data[start:])
	if len(part) != len(full)-skipped {
		t.Fatalf("got %d chunk MACs from offset, want %d", len(part), len(full)-skipped)
	}
	for i := range part {
		if part[i] != full[skipped+i] {
			t.Fatalf("chunk MAC %d differs", i)
		}
	}
}

func TestCTRIVSeeksLikeStreaming(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	key := make([]byte, 16)
	var nonce [8]byte
	rng.Read(key)
	rng.Read(nonce[:])
	plain := make([]byte, 1<<20)
	rng.Read(plain)
	block, _ := aes.NewCipher(key)
	enc := make([]byte, len(plain))
	cipher.NewCTR(block, ctrIV(nonce, 0)).XORKeyStream(enc, plain)

	for _, off := range []int64{16, 4096, 131072, 1<<20 - 16} {
		got := make([]byte, int64(len(enc))-off)
		cipher.NewCTR(block, ctrIV(nonce, off)).XORKeyStream(got, enc[off:])
		if !bytes.Equal(got, plain[off:]) {
			t.Fatalf("decrypting from offset %d failed", off)
		}
	}
}

func TestAttrsRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 16)
	enc := encryptAttrs(key, "Ünïcödé name.mkv")
	a, err := decryptAttrs(key, enc)
	if err != nil || a.Name != "Ünïcödé name.mkv" {
		t.Fatalf("got %+v, %v", a, err)
	}
	if _, err := decryptAttrs(bytes.Repeat([]byte{8}, 16), enc); err == nil {
		t.Fatal("wrong key should fail")
	}
}

func TestCondense(t *testing.T) {
	var m [16]byte
	for i := range m {
		m[i] = byte(i)
	}
	got := condense(m)
	want := [8]byte{0 ^ 4, 1 ^ 5, 2 ^ 6, 3 ^ 7, 8 ^ 12, 9 ^ 13, 10 ^ 14, 11 ^ 15}
	if got != want {
		t.Fatalf("condense = %v, want %v", got, want)
	}
}

// encryptAttrs builds an attribute blob the way MEGA clients do.
func encryptAttrs(key []byte, name string) string {
	plain := []byte(`MEGA{"n":"` + name + `"}`)
	for len(plain)%16 != 0 {
		plain = append(plain, 0)
	}
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(out, plain)
	return b64enc(out)
}
