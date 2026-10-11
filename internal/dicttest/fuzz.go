package dicttest

import (
	"bytes"
	"fmt"
	"reflect"
	"strconv"
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
