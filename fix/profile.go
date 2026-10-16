package fix

import (
	"fmt"
	"slices"
)

// DataPair names a DATA field and the LENGTH field that precedes it and gives
// its size in bytes.
type DataPair struct {
	Length, Data int
}

// InRange reports whether both tags of p are positive and below 2^20, the
// range NewProfile accepts.
func (p DataPair) InRange() bool {
	return p.Length > 0 && p.Length < maxProfileTag && p.Data > 0 && p.Data < maxProfileTag
}

// maxProfileTag bounds the tags a Profile classifies with its lookup table:
// one int32 per tag up to the highest one, 4 MiB at most.
const maxProfileTag = 1 << 20

// Profile tells the parser which fields are DATA fields, read by the length
// their LENGTH field gives because their value may contain SOH. Dictionaries
// disagree on some tag numbers (see dataFields), so a session parses with the
// profile of its own dictionary rather than a process-wide table. A Profile
// is immutable and safe for concurrent use.
type Profile struct {
	// kinds classifies tags with a single lookup: a LENGTH tag maps to the
	// DATA tag it announces, a DATA tag to isDataTag, any other tag to 0.
	kinds []int32
	pairs []DataPair
}

// NewProfile returns the profile of the given DATA fields. Every tag must be
// positive and below 2^20, and appear in a single pair.
func NewProfile(pairs []DataPair) (*Profile, error) {
	maxTag := 0
	for _, p := range pairs {
		for _, t := range []int{p.Length, p.Data} {
			if t <= 0 || t >= maxProfileTag {
				return nil, fmt.Errorf("fix2: NewProfile: tag %d of pair %d/%d out of range 1-%d", t, p.Length, p.Data, maxProfileTag-1)
			}
			maxTag = max(maxTag, t)
		}
		if p.Length == p.Data {
			return nil, fmt.Errorf("fix2: NewProfile: tag %d is both the LENGTH and the DATA field", p.Length)
		}
	}
	kinds := make([]int32, maxTag+1)
	for _, p := range pairs {
		for _, t := range []int{p.Length, p.Data} {
			if kinds[t] != 0 {
				return nil, fmt.Errorf("fix2: NewProfile: tag %d appears in more than one pair", t)
			}
		}
		kinds[p.Length] = int32(p.Data)
		kinds[p.Data] = isDataTag
	}
	return &Profile{kinds: kinds, pairs: slices.Clone(pairs)}, nil
}

// defaultProfile holds the standard DATA fields of dataFields.
var defaultProfile = func() *Profile {
	pairs := make([]DataPair, len(dataFields))
	for i, p := range dataFields {
		pairs[i] = DataPair{Length: p.length, Data: p.data}
	}
	p, err := NewProfile(pairs)
	if err != nil {
		panic(err)
	}
	return p
}()

// DefaultProfile returns the profile of the standard FIX DATA fields, which
// Parse and ParseFromReader use.
func DefaultProfile() *Profile { return defaultProfile }

// DataPairs returns the DATA fields of p.
func (p *Profile) DataPairs() []DataPair { return slices.Clone(p.pairs) }

// DataTag returns the DATA tag announced by LENGTH tag lengthTag, or 0 if
// lengthTag is not a LENGTH tag of p.
func (p *Profile) DataTag(lengthTag int) int {
	if k := p.kind(lengthTag); k > 0 {
		return k
	}
	return 0
}

// DataLengthTag returns the LENGTH tag that announces DATA tag dataTag, or 0
// if dataTag is not a DATA tag of p.
func (p *Profile) DataLengthTag(dataTag int) int {
	if p.kind(dataTag) != isDataTag {
		return 0
	}
	for _, d := range p.pairs {
		if d.Data == dataTag {
			return d.Length
		}
	}
	return 0
}

// kind classifies tag: the DATA tag it announces for a LENGTH tag, isDataTag
// for a DATA tag, 0 otherwise.
func (p *Profile) kind(tag int) int {
	if tag < 0 || tag >= len(p.kinds) {
		return 0
	}
	return int(p.kinds[tag])
}
