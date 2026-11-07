package voice

import (
	"bufio"
	"errors"
	"io"
)

// oggPackets reads an Ogg stream and returns its packets one by one. Unlike pion's
// oggreader, which hands out whole pages, it splits pages into packets (a page may hold
// several) and joins packets that span pages, using the lacing values.
type oggPackets struct {
	r       *bufio.Reader
	pending [][]byte
	partial []byte
}

func newOggPackets(r io.Reader) *oggPackets {
	return &oggPackets{r: bufio.NewReaderSize(r, 8192)}
}

var errNotOgg = errors.New("ogg: página inválida")

// next returns the next packet.
func (o *oggPackets) next() ([]byte, error) {
	for len(o.pending) == 0 {
		if err := o.page(); err != nil {
			return nil, err
		}
	}
	p := o.pending[0]
	o.pending = o.pending[1:]
	return p, nil
}

func (o *oggPackets) page() error {
	var h [27]byte
	if _, err := io.ReadFull(o.r, h[:]); err != nil {
		return err
	}
	if string(h[:4]) != "OggS" {
		return errNotOgg
	}
	segs := make([]byte, h[26])
	if _, err := io.ReadFull(o.r, segs); err != nil {
		return err
	}
	size := 0
	for _, s := range segs {
		size += int(s)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(o.r, body); err != nil {
		return err
	}
	at := 0
	for _, s := range segs {
		o.partial = append(o.partial, body[at:at+int(s)]...)
		at += int(s)
		// A lacing value under 255 ends a packet; 255 means it goes on.
		if s < 255 {
			o.pending = append(o.pending, o.partial)
			o.partial = nil
		}
	}
	return nil
}
