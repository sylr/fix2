package main

import (
	"encoding/binary"
	"fmt"
	"time"
)

// The message the client sends and the server echoes: SBE 1.0, little
// endian, schema 100, template 1, version 0. Its block holds its number
// and its send time, in nanoseconds since the epoch, then padding up to
// the size asked for.
const (
	schemaID   = 100
	templateID = 1
	headerLen  = 8  // SBE's standard message header
	minBlock   = 16 // number and send time
	minSize    = headerLen + minBlock
	maxSize    = headerLen + 0xffff // the header's block length is a uint16
)

// probe is a message to send.
type probe struct {
	id   uint64
	sent time.Time
	size int // of the message, header included, at least minSize
}

// AppendTo implements session.Outgoing.
func (p probe) AppendTo(dst []byte) ([]byte, error) {
	if p.size < minSize || p.size > maxSize {
		return nil, fmt.Errorf("message size %d out of [%d, %d]", p.size, minSize, maxSize)
	}
	n := len(dst)
	dst = append(dst, make([]byte, p.size)...)
	b := dst[n:]
	binary.LittleEndian.PutUint16(b[0:], uint16(p.size-headerLen))
	binary.LittleEndian.PutUint16(b[2:], templateID)
	binary.LittleEndian.PutUint16(b[4:], schemaID)
	binary.LittleEndian.PutUint16(b[6:], 0)
	binary.LittleEndian.PutUint64(b[8:], p.id)
	binary.LittleEndian.PutUint64(b[16:], uint64(p.sent.UnixNano()))
	return dst, nil
}

// readProbe reads a message the server echoed.
func readProbe(msg []byte) (id uint64, sent time.Time, err error) {
	if len(msg) < minSize {
		return 0, time.Time{}, fmt.Errorf("%d bytes, shorter than a probe", len(msg))
	}
	block := int(binary.LittleEndian.Uint16(msg[0:]))
	template := binary.LittleEndian.Uint16(msg[2:])
	schema := binary.LittleEndian.Uint16(msg[4:])
	if schema != schemaID || template != templateID || block < minBlock || headerLen+block > len(msg) {
		return 0, time.Time{}, fmt.Errorf("not a probe: schema %d, template %d, block %d, %d bytes", schema, template, block, len(msg))
	}
	id = binary.LittleEndian.Uint64(msg[8:])
	sent = time.Unix(0, int64(binary.LittleEndian.Uint64(msg[16:])))
	return id, sent, nil
}
