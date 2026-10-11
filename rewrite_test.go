package fix2

import (
	"bytes"
	"errors"
	"testing"
)

func TestRewriteForResend(t *testing.T) {
	now := []byte("20261011-09:00:00.000")
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "adds PossDupFlag and OrigSendingTime in header order",
			body: "35=D\x0134=5\x0149=A\x0152=20261011-08:00:00.000\x0156=B\x0111=ORD\x0154=1\x01",
			want: "35=D\x0134=5\x0143=Y\x0149=A\x0152=20261011-09:00:00.000\x0156=B\x01122=20261011-08:00:00.000\x0111=ORD\x0154=1\x01",
		},
		{
			name: "replaces PossDupFlag, keeps OrigSendingTime",
			body: "35=D\x0134=5\x0143=N\x0149=A\x0152=T2\x0156=B\x01122=T1\x0111=ORD\x01",
			want: "35=D\x0134=5\x0143=Y\x0149=A\x0152=20261011-09:00:00.000\x0156=B\x01122=T1\x0111=ORD\x01",
		},
		{
			name: "no body",
			body: "35=0\x0134=5\x0149=A\x0152=T\x0156=B\x01",
			want: "35=0\x0134=5\x0143=Y\x0149=A\x0152=20261011-09:00:00.000\x0156=B\x01122=T\x01",
		},
		{
			name: "header after its standard order, hops group",
			body: "35=D\x0149=A\x0156=B\x0134=5\x0152=T\x01627=1\x01628=H\x0111=ORD\x01",
			want: "35=D\x0143=Y\x0149=A\x0156=B\x0134=5\x0152=20261011-09:00:00.000\x01122=T\x01627=1\x01628=H\x0111=ORD\x01",
		},
		{
			name: "SendingTime and PossDupFlag look-alikes in the body and a DATA field are copied",
			body: "35=D\x0134=5\x0149=A\x0152=T\x0156=B\x0111=ORD\x0195=9\x0196=52=X\x0143=N\x01",
			want: "35=D\x0134=5\x0143=Y\x0149=A\x0152=20261011-09:00:00.000\x0156=B\x01122=T\x0111=ORD\x0195=9\x0196=52=X\x0143=N\x01",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildMsg("FIX.4.4", tc.body)
			got, err := RewriteForResend([]byte("prefix"), raw, nil, now)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]byte("prefix"), buildMsg("FIX.4.4", tc.want)...)
			if !bytes.Equal(got, want) {
				t.Errorf("RewriteForResend =\n%q\nwant\n%q", got, want)
			}
			if _, err := Parse(got[len("prefix"):]); err != nil {
				t.Errorf("rewritten message does not parse: %v", err)
			}
		})
	}
}

func TestRewriteForResend_Errors(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"does not parse", []byte("garbage that is long enough to try"), ErrInvalidBeginTag},
		{"no SendingTime", buildMsg("FIX.4.4", "35=D\x0134=5\x0111=ORD\x01"), ErrInvalidField},
		{"SendingTime after a body field", buildMsg("FIX.4.4", "35=D\x0154=5\x0149=A\x0152=T\x01"), ErrInvalidField},
		{"signed", buildMsg("FIX.4.4", "35=D\x0134=5\x0152=T\x0111=ORD\x0193=3\x0189=abc\x01"), ErrSignedMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := []byte("prefix")
			got, err := RewriteForResend(dst, tc.raw, nil, []byte("now"))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if string(got) != "prefix" {
				t.Errorf("dst changed on error: %q", got)
			}
		})
	}
}

func TestRewriteForResend_DoesNotAllocate(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under the race detector")
	}
	raw := buildMsg("FIX.4.4", "35=D\x0134=5\x0149=A\x0152=20261011-08:00:00.000\x0156=B\x0111=ORD\x0154=1\x01")
	now := []byte("20261011-09:00:00.000")
	buf := make([]byte, 0, 512)
	allocs := testing.AllocsPerRun(100, func() {
		var err error
		if buf, err = RewriteForResend(buf[:0], raw, nil, now); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("RewriteForResend: %v allocs, want 0", allocs)
	}
}

func FuzzRewriteForResend(f *testing.F) {
	f.Add(buildMsg("FIX.4.4", "35=D\x0134=5\x0149=A\x0152=T\x0156=B\x0111=ORD\x01"))
	f.Add(buildMsg("FIX.4.4", "35=D\x0143=N\x0152=T\x01122=O\x0195=3\x0196=a\x01b\x01"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		out, err := RewriteForResend(nil, raw, nil, []byte("NOW"))
		if err != nil {
			return
		}
		m, err := Parse(out)
		if err != nil {
			t.Fatalf("rewrite of %q does not parse: %v\n%q", raw, err, out)
		}
		orig, _ := Parse(raw)
		if string(m.Get(52)) != "NOW" || string(m.Get(43)) != "Y" || m.Get(122) == nil {
			t.Fatalf("rewrite of %q: 52=%q 43=%q 122=%q", raw, m.Get(52), m.Get(43), m.Get(122))
		}
		if m.Count() < orig.Count() {
			t.Fatalf("rewrite of %q lost fields", raw)
		}
	})
}
