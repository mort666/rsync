package rsynctoken

// A generator of test cases for the token streams, from the compression
// spike (Phase 0b): a basis file, a new file built from it, and the
// send_token calls a sender would make for the new file.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strings"
)

// Op is one send_token() call: token, literal length before it, and the
// matched block length (toklen).
type Op struct {
	Token  int32
	N      int
	TokLen int
}

// Case is a generated basis file, a new file built from it, and the token
// calls a sender would make to describe the new file.
type Case struct {
	Basis    []byte
	New      []byte
	BlockLen int
	Ops      []Op
}

var words = strings.Fields(`rsync protocol token deflate inflate history window
block match literal sender receiver generator checksum compress negotiate
varint flist index multiplex flush sync dictionary stream gokrazy upstream
the a of and to in is it that for on with as by this be are from`)

func text(r *rand.Rand, n int) []byte {
	var b []byte
	for len(b) < n {
		b = append(b, words[r.IntN(len(words))]...)
		if r.IntN(12) == 0 {
			b = append(b, '\n')
		} else {
			b = append(b, ' ')
		}
	}
	return b[:n]
}

func noise(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// GenCase builds a test case. The new file mixes matched basis blocks, runs
// of consecutive blocks, compressible text, incompressible noise, and,
// importantly for "zlib", literal data copied out of recently matched blocks.
// That last kind is where rsync's zlib variant gets back-references into
// matched data, so it is what proves the history handling.
func GenCase(seed uint64, size, blockLen int) *Case {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	c := &Case{BlockLen: blockLen}
	c.Basis = text(r, size)
	if r.IntN(2) == 0 {
		// Sometimes a basis with a remainder block.
		c.Basis = append(c.Basis, text(r, 1+r.IntN(blockLen-1))...)
	}
	count := (len(c.Basis) + blockLen - 1) / blockLen
	block := func(i int) []byte {
		end := min((i+1)*blockLen, len(c.Basis))
		return c.Basis[i*blockLen : end]
	}

	pending := 0
	var lastMatched []byte
	addLiteral := func(p []byte) {
		c.New = append(c.New, p...)
		pending += len(p)
		// Upstream's match.c sends literal-only data (token -2) once enough
		// accumulates. Any split is valid input to send_token, so split at
		// random points past 32 KiB.
		for pending > 32*1024+blockLen {
			n := 32*1024 + r.IntN(blockLen)
			c.Ops = append(c.Ops, Op{Token: -2, N: n})
			pending -= n
		}
	}
	addToken := func(i int) {
		b := block(i)
		c.Ops = append(c.Ops, Op{Token: int32(i), N: pending, TokLen: len(b)})
		c.New = append(c.New, b...)
		pending = 0
		lastMatched = b
	}

	for len(c.New) < size {
		switch k := r.IntN(10); {
		case k < 3:
			addToken(r.IntN(count))
		case k < 4:
			start := r.IntN(count)
			for i := start; i < min(count, start+2+r.IntN(70)); i++ {
				addToken(i)
			}
		case k < 6:
			addLiteral(text(r, 1+r.IntN(3*blockLen)))
		case k < 7:
			addLiteral(noise(r, 1+r.IntN(blockLen)))
		default:
			if lastMatched == nil {
				continue
			}
			// Copy part of the last matched block, slightly changed.
			from := r.IntN(len(lastMatched))
			to := min(len(lastMatched), from+16+r.IntN(2048))
			p := append([]byte(nil), lastMatched[from:to]...)
			if len(p) > 0 {
				p[r.IntN(len(p))] ^= 0x20
			}
			addLiteral(p)
		}
	}
	c.Ops = append(c.Ops, Op{Token: -1, N: pending})
	return c
}

// Encode runs the ops through e.
func (c *Case) Encode(e Encoder) error {
	pos := 0
	for _, op := range c.Ops {
		lit := c.New[pos : pos+op.N]
		pos += op.N
		var tok []byte
		if op.Token >= 0 {
			tok = c.New[pos : pos+op.TokLen]
			pos += op.TokLen
		}
		if err := e.SendToken(op.Token, lit, tok); err != nil {
			return err
		}
	}
	return nil
}

// Decode rebuilds a new file from one file's tokens and the basis, as
// receiver.c:receive_data does.
func Decode(d Decoder, basis []byte, blockLen int) ([]byte, error) {
	count := (len(basis) + blockLen - 1) / blockLen
	var out []byte
	for {
		lit, tok, err := d.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		if lit != nil {
			out = append(out, lit...)
			continue
		}
		if tok < 0 || int(tok) >= count {
			return out, fmt.Errorf("block index %d out of range (count %d)", tok, count)
		}
		b := basis[int(tok)*blockLen : min((int(tok)+1)*blockLen, len(basis))]
		if w := d.See(int64(len(b))); w != nil {
			// In pieces, as the receiver copies a block.
			for p := b; len(p) > 0; p = p[min(len(p), 5000):] {
				w.Write(p[:min(len(p), 5000)])
			}
		}
		out = append(out, b...)
	}
}

func (c *Case) WriteFiles(dir string) error {
	if err := os.WriteFile(dir+"/basis", c.Basis, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(dir+"/new", c.New, 0o644); err != nil {
		return err
	}
	f, err := os.Create(dir + "/ops")
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, op := range c.Ops {
		fmt.Fprintf(w, "%d %d %d\n", op.Token, op.N, op.TokLen)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// GenLongRun builds a case whose new file is mostly one unbroken run of
// consecutive matched blocks, long enough that upstream must split it at
// 65,536 tokens (send_deflated_token's run_start + 65536 check). A literal
// in the middle and a jump to a far block index exercise TOKEN_LONG too.
func GenLongRun(blocks, blockLen int) *Case {
	r := rand.New(rand.NewPCG(7, 11))
	c := &Case{BlockLen: blockLen, Basis: text(r, blocks*blockLen)}
	block := func(i int) []byte { return c.Basis[i*blockLen : (i+1)*blockLen] }
	pending := 0
	add := func(i int) {
		c.Ops = append(c.Ops, Op{Token: int32(i), N: pending, TokLen: blockLen})
		c.New = append(c.New, block(i)...)
		pending = 0
	}
	for i := 0; i < blocks*3/4; i++ {
		add(i)
	}
	lit := text(r, 1000)
	c.New = append(c.New, lit...)
	pending = len(lit)
	add(5) // big backwards jump: TOKEN_LONG
	for i := 6; i < blocks; i++ {
		add(i)
	}
	c.Ops = append(c.Ops, Op{Token: -1, N: 0})
	return c
}
