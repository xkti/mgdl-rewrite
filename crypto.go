package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// MEGA encrypts every file with AES-128 in CCM-like fashion: CTR mode for
// confidentiality, and a CBC-MAC computed over the plaintext in variable-size
// "MAC chunks" for integrity. The per-chunk MACs are then folded together with a
// second CBC pass into a 16-byte meta-MAC, of which 8 condensed bytes are stored
// in the last quarter of the node key. See the MEGA Security Whitepaper.

// macChunkBase is the unit of MEGA's MAC chunk sizes: the first chunks are 1, 2,
// 3 … 8 × 128 KiB, and every chunk after the eighth is 1 MiB.
const macChunkBase = 128 * 1024

// macSteadyStart is where the MAC chunks settle into a fixed 1 MiB stride: the
// sum of the first eight chunk sizes, (1+2+…+8) × 128 KiB = 4.5 MiB.
const macSteadyStart = 36 * macChunkBase

// macSteadyStep is the size of every MAC chunk from macSteadyStart onwards.
const macSteadyStep = 8 * macChunkBase

// b64dec decodes MEGA's URL-safe, unpadded base64. It also accepts the standard
// alphabet and stray padding, so keys pasted from older tools still work.
func b64dec(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return base64.RawURLEncoding.DecodeString(s)
}

// b64enc encodes in MEGA's URL-safe, unpadded base64.
func b64enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// fileKey is the unpacked 32-byte node key of a file.
type fileKey struct {
	raw   [32]byte // the key exactly as MEGA stores it
	aes   [16]byte // k[0:16] XOR k[16:32]: the AES key for content and attributes
	nonce [8]byte  // k[16:24]: the CTR nonce, also seeds every chunk MAC
	mac   [8]byte  // k[24:32]: the expected condensed meta-MAC
}

// unpackFileKey splits a 32-byte node key into its AES key, nonce and meta-MAC.
func unpackFileKey(k []byte) (*fileKey, error) {
	if len(k) != 32 {
		return nil, fmt.Errorf("file key must be 32 bytes, got %d", len(k))
	}
	fk := &fileKey{}
	copy(fk.raw[:], k)
	for i := range 16 {
		fk.aes[i] = k[i] ^ k[i+16]
	}
	copy(fk.nonce[:], k[16:24])
	copy(fk.mac[:], k[24:32])
	return fk, nil
}

// decryptNodeKey unwraps a node key (16 bytes for folders, 32 for files) that was
// encrypted with AES-128-ECB under the share's master key.
func decryptNodeKey(master, enc []byte) ([]byte, error) {
	if len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("encrypted key has bad length %d", len(enc))
	}
	block, err := aes.NewCipher(master)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(enc))
	for i := 0; i < len(enc); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], enc[i:i+aes.BlockSize])
	}
	return out, nil
}

// nodeAttrs is the decrypted attribute blob. Only the name is used.
type nodeAttrs struct {
	Name string `json:"n"`
}

// decryptAttrs decrypts a node's attribute blob: AES-128-CBC with a zero IV, the
// plaintext being "MEGA" followed by JSON and zero padding.
func decryptAttrs(key []byte, encB64 string) (*nodeAttrs, error) {
	enc, err := b64dec(encB64)
	if err != nil {
		return nil, fmt.Errorf("attributes: %w", err)
	}
	if len(enc) == 0 || len(enc)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("attributes have bad length %d", len(enc))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(enc))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(plain, enc)
	if !bytes.HasPrefix(plain, []byte("MEGA")) {
		return nil, errors.New("attributes did not decrypt")
	}
	plain = bytes.TrimRight(plain[4:], "\x00")
	var a nodeAttrs
	if err := json.Unmarshal(plain, &a); err != nil {
		return nil, fmt.Errorf("attributes: %w", err)
	}
	return &a, nil
}

// ctrIV returns the CTR counter block for a byte offset: the 8-byte nonce followed
// by the 64-bit big-endian block index. offset must be a multiple of 16, which every
// chunk start and checkpoint is (they all sit on MAC chunk boundaries).
func ctrIV(nonce [8]byte, offset int64) []byte {
	iv := make([]byte, aes.BlockSize)
	copy(iv, nonce[:])
	binary.BigEndian.PutUint64(iv[8:], uint64(offset/aes.BlockSize))
	return iv
}

// macChunkEnd returns the end (exclusive) of the MAC chunk that starts at start.
// start must itself be a MAC chunk boundary.
func macChunkEnd(start int64) int64 {
	if start >= macSteadyStart {
		return start + macSteadyStep
	}
	// Walk the eight growing chunks; at most eight iterations.
	var pos int64
	for i := int64(1); i <= 8; i++ {
		pos += i * macChunkBase
		if pos > start {
			return pos
		}
	}
	return macSteadyStart + macSteadyStep // unreachable for valid boundaries
}

// macBoundaryAtOrAfter returns the smallest MAC chunk boundary >= x.
func macBoundaryAtOrAfter(x int64) int64 {
	if x <= 0 {
		return 0
	}
	if x >= macSteadyStart {
		steps := (x - macSteadyStart + macSteadyStep - 1) / macSteadyStep
		return macSteadyStart + steps*macSteadyStep
	}
	var pos int64
	for i := int64(1); i <= 8; i++ {
		pos += i * macChunkBase
		if pos >= x {
			return pos
		}
	}
	return macSteadyStart
}

// macBoundaryFloor returns the largest MAC boundary <= pos, or end if pos >= end.
func macBoundaryFloor(pos, end int64) int64 {
	if pos >= end {
		return end
	}
	if pos >= macSteadyStart {
		return macSteadyStart + (pos-macSteadyStart)/macSteadyStep*macSteadyStep
	}
	var b int64
	for next := macChunkEnd(0); next <= pos; next = macChunkEnd(b) {
		b = next
	}
	return b
}

// macStream computes the CBC-MAC of one stream of plaintext as it arrives,
// emitting one 16-byte MAC per MEGA MAC chunk. It is positioned at a MAC chunk
// boundary when created and can be fed arbitrarily sized writes.
//
// Each chunk MAC is CBC-MAC with IV = nonce‖nonce over the chunk's bytes, the last
// block zero-padded. CBC encryption's final ciphertext block is exactly that MAC,
// so the stdlib CBC encrypter does the heavy lifting.
type macStream struct {
	block cipher.Block
	nonce [8]byte

	pos      int64 // absolute file offset of the next byte
	chunkEnd int64 // end of the MAC chunk currently being computed
	fileEnd  int64 // file size; the final chunk is cut short here

	lastBoundary int64 // end of the most recently completed MAC chunk

	mode    cipher.BlockMode
	last    [16]byte // latest CBC output block = running MAC of the current chunk
	pending [16]byte // bytes not yet forming a full block
	npend   int
	scratch []byte
}

// newMACStream starts computing MACs at offset start (a MAC boundary) of a file of
// the given size.
func newMACStream(key [16]byte, nonce [8]byte, start, size int64) *macStream {
	block, _ := aes.NewCipher(key[:]) // a 16-byte key cannot fail
	m := &macStream{block: block, nonce: nonce, pos: start, fileEnd: size, lastBoundary: start, scratch: make([]byte, 64*1024)}
	m.resetChunk()
	return m
}

// seek repositions the stream at start, a MAC boundary, dropping any chunk in
// progress, so one stream (and its scratch buffer) can compute many chunk MACs.
func (m *macStream) seek(start int64) {
	m.pos, m.lastBoundary = start, start
	m.resetChunk()
}

func (m *macStream) resetChunk() {
	var iv [16]byte
	copy(iv[:8], m.nonce[:])
	copy(iv[8:], m.nonce[:])
	m.mode = cipher.NewCBCEncrypter(m.block, iv[:])
	m.last = iv
	m.npend = 0
	m.chunkEnd = min(macChunkEnd(m.pos), m.fileEnd)
}

// cbc runs whole blocks through the encrypter, remembering the last output block.
func (m *macStream) cbc(p []byte) {
	for len(p) > 0 {
		n := min(len(p), len(m.scratch))
		m.mode.CryptBlocks(m.scratch[:n], p[:n])
		copy(m.last[:], m.scratch[n-16:n])
		p = p[n:]
	}
}

// finishChunk pads the trailing partial block, if any, and returns the chunk MAC.
func (m *macStream) finishChunk() [16]byte {
	if m.npend > 0 {
		for i := m.npend; i < 16; i++ {
			m.pending[i] = 0
		}
		m.cbc(m.pending[:])
		m.npend = 0
	}
	return m.last
}

// Write feeds plaintext. It returns the MACs of every chunk completed by p, in
// order. p must not extend past the end of the file.
func (m *macStream) Write(p []byte) [][16]byte {
	var done [][16]byte
	for len(p) > 0 {
		// Only feed up to the end of the current chunk.
		room := m.chunkEnd - m.pos
		take := min(int64(len(p)), room)
		q := p[:take]
		p = p[take:]
		m.pos += take

		// Top up a pending partial block first.
		if m.npend > 0 {
			n := copy(m.pending[m.npend:], q)
			m.npend += n
			q = q[n:]
			if m.npend == 16 {
				m.cbc(m.pending[:])
				m.npend = 0
			}
		}
		if full := len(q) &^ 15; full > 0 {
			m.cbc(q[:full])
			q = q[full:]
		}
		if len(q) > 0 {
			m.npend = copy(m.pending[:], q)
		}

		if m.pos == m.chunkEnd {
			done = append(done, m.finishChunk())
			m.lastBoundary = m.pos
			if m.pos < m.fileEnd {
				m.resetChunk()
			}
		}
	}
	return done
}

// fileMeta computes the meta-MAC of a whole file read from r (--verify-existing).
// MAC chunks are independent CBC chains, so up to workers of them are read and
// MACed at once; only the fold over their MACs is serial, done here strictly in
// file order. progress gets the size of each chunk once it is MACed (from the
// workers, so it must be safe for concurrent use). A file shorter than size is an
// error, not a mismatch.
func fileMeta(ctx context.Context, r io.ReaderAt, size int64, key [16]byte, nonce [8]byte, workers int, progress func(int64)) ([16]byte, error) {
	type result struct {
		mac [16]byte
		err error
	}
	type piece struct {
		start, end int64
		out        chan result
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait() // nobody touches r once we return
	}()
	workers = max(workers, 1)
	jobs := make(chan piece)
	// order holds each piece's result channel in file order; its capacity bounds
	// how far the workers may run ahead of the fold.
	order := make(chan chan result, 2*workers)

	wg.Go(func() {
		defer close(jobs)
		defer close(order)
		for s := int64(0); s < size; {
			e := min(macChunkEnd(s), size)
			out := make(chan result, 1)
			select {
			case order <- out:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- piece{s, e, out}:
			case <-ctx.Done():
				return
			}
			s = e
		}
	})
	for range workers {
		wg.Go(func() {
			m := newMACStream(key, nonce, 0, size)
			buf := make([]byte, macSteadyStep) // the largest MAC chunk
			for p := range jobs {
				if ctx.Err() != nil {
					p.out <- result{err: ctx.Err()}
					continue
				}
				b := buf[:p.end-p.start]
				if n, err := r.ReadAt(b, p.start); n < len(b) {
					if err == io.EOF {
						err = io.ErrUnexpectedEOF
					}
					p.out <- result{err: fmt.Errorf("reading at %d: %w", p.start, err)}
					continue
				}
				m.seek(p.start)
				macs := m.Write(b)
				p.out <- result{mac: macs[0]} // b is exactly one MAC chunk
				progress(int64(len(b)))
			}
		})
	}

	fold := newMetaFold(key, [16]byte{})
	for out := range order {
		select {
		case res := <-out:
			if res.err != nil {
				return [16]byte{}, res.err
			}
			fold.add(res.mac)
		case <-ctx.Done():
			return [16]byte{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return [16]byte{}, err
	}
	return fold.state, nil
}

// metaFold is the running second-level CBC over chunk MACs (zero IV).
type metaFold struct {
	block cipher.Block
	state [16]byte
}

func newMetaFold(key [16]byte, state [16]byte) *metaFold {
	block, _ := aes.NewCipher(key[:])
	return &metaFold{block: block, state: state}
}

// add folds one chunk MAC into the meta-MAC.
func (f *metaFold) add(cmac [16]byte) { foldMAC(f.block, &f.state, cmac) }

// foldMAC is one step of the meta-MAC's CBC: state = E(state XOR cmac).
func foldMAC(block cipher.Block, state *[16]byte, cmac [16]byte) {
	for i := range state {
		state[i] ^= cmac[i]
	}
	block.Encrypt(state[:], state[:])
}

// condense packs the 16-byte meta-MAC into the 8 bytes stored in the node key.
func condense(meta [16]byte) [8]byte {
	return [8]byte{
		meta[0] ^ meta[4], meta[1] ^ meta[5], meta[2] ^ meta[6], meta[3] ^ meta[7],
		meta[8] ^ meta[12], meta[9] ^ meta[13], meta[10] ^ meta[14], meta[11] ^ meta[15],
	}
}
