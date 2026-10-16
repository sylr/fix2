package rich

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// realLogic returns the messages Real Logic's SBE codecs encoded, by name,
// from a fixture of testdata/reallogic.
func realLogic(t testing.TB, file string) map[string][]byte {
	t.Helper()
	f, err := os.Open("../testdata/reallogic/" + file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	msgs := map[string][]byte{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, h, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		msgs[name] = b
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return msgs
}

// The messages Real Logic's SBE 1.33.0 encodes from rich.xml, with the
// values of full (see testdata/reallogic/RichCheck.java.txt), decode to
// those values and encode back to the same bytes.
func TestRealLogic(t *testing.T) {
	msgs := realLogic(t, "rich.hex")
	var got Order
	if err := got.Decode(msgs["order"]); err != nil {
		t.Fatal(err)
	}
	if !same(&got, full()) {
		t.Errorf("decoded\n%+v\nwant\n%+v", got, *full())
	}
	if b := encode(t, full()); !bytes.Equal(b, msgs["order"]) {
		t.Errorf("encoded\n%x\nReal Logic\n%x", b, msgs["order"])
	}
	var p Ping
	if err := p.Decode(msgs["ping"]); err != nil {
		t.Fatal(err)
	}
	if b, _ := p.AppendTo(nil); !bytes.Equal(b, msgs["ping"]) {
		t.Errorf("ping %x, Real Logic %x", b, msgs["ping"])
	}
}
