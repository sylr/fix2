package dicttest

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"

	"github.com/sylr/fix2/fix/dictionary"
)

// SOH returns s with '|' as SOH, for readable seeds of fuzz targets.
func SOH(s string) []byte { return bytes.ReplaceAll([]byte(s), []byte("|"), []byte{1}) }

// Frame returns a message of the given BeginString around body, which holds
// MsgType and what follows, with the BodyLength and CheckSum that make it
// parse.
func Frame(begin string, body []byte) []byte {
	if len(body) == 0 || body[len(body)-1] != 1 {
		body = append(bytes.Clone(body), 1)
	}
	msg := []byte("8=" + begin + "\x019=" + strconv.Itoa(len(body)) + "\x01")
	msg = append(msg, body...)
	var sum byte
	for _, c := range msg {
		sum += c
	}
	return append(msg, fmt.Sprintf("10=%03d\x01", sum)...)
}

// Body returns the part of a message Frame takes: from MsgType to before
// CheckSum. It returns nil if wire is not a message.
func Body(wire []byte) []byte {
	parts := bytes.SplitN(wire, []byte{1}, 3)
	if len(parts) < 3 {
		return nil
	}
	rest := parts[2]
	if i := bytes.LastIndex(rest, []byte("\x0110=")); i >= 0 {
		return rest[:i+1]
	}
	return rest
}

// Full returns the body of a message of type msgType, one of d's, as Frame
// takes it, holding the fields d defines for it, its header and its
// trailer, so that every getter finds its field, whatever it makes of the
// value: "1", "x" for a DATA field after its LENGTH field. A tag a group
// holds is written in its entries only. Each group has two entries: one with
// its fields and its nested groups, one with its delimiter, unless a DATA
// field or a group, and a tag no message defines, which the entry keeps.
func Full(d *dictionary.Dict, msgType string) []byte {
	mdef := d.Messages[msgType]
	if mdef == nil {
		panic("dicttest: no message of type " + msgType)
	}
	if d.Fields[undefinedTag] != nil {
		panic("dicttest: the dictionary defines tag " + strconv.Itoa(undefinedTag))
	}
	b := []byte("35=" + msgType + "|")
	p := d.Profile()
	// field writes tag, and the DATA field a LENGTH tag announces; a DATA
	// tag is written after its LENGTH tag only.
	field := func(tag int) {
		switch {
		case tag == 8 || tag == 9 || tag == 35 || tag == 10, p.DataLengthTag(tag) != 0:
		case p.DataTag(tag) != 0:
			b = append(b, strconv.Itoa(tag)+"=1|"+strconv.Itoa(p.DataTag(tag))+"=x|"...)
		default:
			b = append(b, strconv.Itoa(tag)+"=1|"...)
		}
	}
	// The tags of group entries, written in their groups only.
	inGroup := map[int]bool{}
	var mark func(gs []dictionary.GroupDef)
	mark = func(gs []dictionary.GroupDef) {
		for _, g := range gs {
			inGroup[g.CounterTag] = true
			for _, tag := range g.Layout {
				inGroup[tag] = true
			}
			mark(g.Groups)
		}
	}
	mark(d.HeaderGroups)
	mark(mdef.Groups)
	mark(d.TrailerGroups)
	written := map[int]bool{}
	fields := func(refs []dictionary.FieldRef) {
		for _, f := range refs {
			if !inGroup[f.Tag] && !written[f.Tag] {
				written[f.Tag] = true
				field(f.Tag)
			}
		}
	}
	var group func(g dictionary.GroupDef)
	group = func(g dictionary.GroupDef) {
		b = append(b, strconv.Itoa(g.CounterTag)+"=2|"...)
		for _, tag := range g.Layout {
			if i := slices.IndexFunc(g.Groups, func(n dictionary.GroupDef) bool { return n.CounterTag == tag }); i >= 0 {
				group(g.Groups[i])
			} else {
				field(tag)
			}
		}
		if !slices.ContainsFunc(g.Groups, func(n dictionary.GroupDef) bool { return n.CounterTag == g.DelimTag }) {
			field(g.DelimTag)
		}
		b = append(b, strconv.Itoa(undefinedTag)+"=1|"...)
	}
	fields(d.Header)
	for _, g := range d.HeaderGroups {
		group(g)
	}
	fields(mdef.Fields)
	for _, g := range mdef.Groups {
		group(g)
	}
	fields(d.Trailer)
	for _, g := range d.TrailerGroups {
		group(g)
	}
	return SOH(string(b))
}

// undefinedTag is a tag none of the test dictionaries defines: Full checks.
const undefinedTag = 9999

// FullAll returns Full of every message type of d, in MsgType order.
func FullAll(d *dictionary.Dict) [][]byte {
	var bodies [][]byte
	for _, msgType := range slices.Sorted(maps.Keys(d.Messages)) {
		bodies = append(bodies, Full(d, msgType))
	}
	return bodies
}

// maxExerciseDepth bounds how deep Exercise follows group entries.
const maxExerciseDepth = 8

// Exercise calls every exported method of v that takes no arguments, and
// those of the group entries they return, to its depth limit. It is how fuzz
// targets read every getter of a generated type; a panic is the finding.
func Exercise(v any) {
	exercise(reflect.ValueOf(v), 0)
}

func exercise(v reflect.Value, depth int) {
	if depth > maxExerciseDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Slice:
		for i := range v.Len() {
			e := v.Index(i)
			if e.Kind() == reflect.Struct {
				// Methods of generated entries have pointer receivers.
				p := reflect.New(e.Type())
				p.Elem().Set(e)
				e = p
			}
			exercise(e, depth+1)
		}
		return
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
	default:
		return
	}
	t := v.Type()
	for i := range t.NumMethod() {
		m := t.Method(i)
		if m.Type.NumIn() != 1 {
			continue
		}
		for _, out := range v.Method(i).Call(nil) {
			exercise(out, depth+1)
		}
	}
}
