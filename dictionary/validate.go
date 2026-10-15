package dictionary

import (
	"bytes"
	"fmt"
	"slices"

	fix2 "github.com/sylr/fix2"
)

// Compile-time check that Dict implements fix2.Validator.
var _ fix2.Validator = (*Dict)(nil)

// Sentinel errors for dictionary validation.
var (
	ErrValidation       = fmt.Errorf("%w: validation", fix2.ErrFix2)
	ErrUnknownMsgType   = fmt.Errorf("%w: unknown MsgType", ErrValidation)
	ErrRequiredTag      = fmt.Errorf("%w: required tag missing", ErrValidation)
	ErrUnexpectedTag    = fmt.Errorf("%w: unexpected tag", ErrValidation)
	ErrInvalidEnumValue = fmt.Errorf("%w: invalid enum value", ErrValidation)
	ErrEmptyValue       = fmt.Errorf("%w: tag without a value", ErrValidation)
	ErrIncorrectFormat  = fmt.Errorf("%w: incorrect data format", ErrValidation)
	ErrRepeatedTag      = fmt.Errorf("%w: tag appears more than once", ErrValidation)
	ErrOutOfOrder       = fmt.Errorf("%w: tag out of order", ErrValidation)
	ErrGroupCount       = fmt.Errorf("%w: incorrect NumInGroup count", ErrValidation)
	ErrTagOutsideGroup  = fmt.Errorf("%w: tag allowed only in group entries", ErrValidation)
	ErrGroupOrder       = fmt.Errorf("%w: repeating group fields out of order", ErrValidation)
)

// ValidationError wraps a sentinel error with the offending tag, the
// SessionRejectReason a session Reject would carry for it, and a message. Its
// Error text is the message prefixed with "fix2: ".
type ValidationError struct {
	Err    error
	Tag    int // the offending tag: 35 for an unknown MsgType
	Reason RejectReason
	Msg    string
}

func (e *ValidationError) Error() string {
	return fix2.ErrFix2.Error() + ": " + e.Msg
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

// Validate checks msg against the dictionary.
//
// It verifies that:
//  1. MsgType is known
//  2. All required header tags are present
//  3. All required trailer tags are present
//  4. All required message-body tags are present; required fields of an
//     optional component only when the component is present, i.e. when a
//     tag found nowhere else in the message is (components whose presence
//     cannot be told apart this way are not enforced)
//  5. Every tag is allowed (header + trailer + message fields + structural),
//     a tag the message holds only in group entries only there (reason 2)
//  6. Enum values are valid for fields that define enums
//
// Groups are read as GroupsOf reads them: a group holds at most as many
// entries as its counter says, and the fields after them are the message's
// own, one sharing a tag with the group's delimiter included. Required tags
// (2-4) must be present outside group entries: a tag a group entry holds
// does not stand for the message's own. Validate is ValidateWith with
// CheckUnknownTags and CheckUserDefinedTags, which 5 is.
//
// Entries are validated against their group's definition only with
// CheckGroupRequired and CheckGroupOrder (see ValidateWith): Validate lets
// an entry lack its required fields and hold them in any order. A tag an
// entry holds twice ends it, as GroupsOf reads it: the second is rejected
// only where the message does not allow it.
func (d *Dict) Validate(msg *fix2.Message) error {
	return d.validate(msg, checksValidate)
}

// Defines reports whether messages of type msgType may hold tag: one of
// their fields, group entries' included, of the standard header or trailer,
// or the LENGTH or DATA field one of them pairs with. An application
// dictionary of FIX 5.0 or later has no header or trailer of its own but
// BeginString, BodyLength and CheckSum: Compose gives it the session
// dictionary's.
func (d *Dict) Defines(msgType string, tag int) bool {
	mdef, ok := d.Messages[msgType]
	return ok && d.lookup(mdef, tag) != nil
}

// DefinesAny reports whether a message of any type may hold tag, as Defines
// reports it. The entries of the header's and trailer's groups, which every
// message carries, keep the tags it does not report.
func (d *Dict) DefinesAny(tag int) bool {
	return d.anyTags.get(tag) != nil
}

// validate checks msg as Validate does, plus checks (see ValidateWith).
func (d *Dict) validate(msg *fix2.Message, checks Checks) error {
	// 1. MsgType must be known.
	mdef, ok := d.Messages[string(msg.MsgType())]
	if !ok {
		return &ValidationError{
			Err:    ErrUnknownMsgType,
			Tag:    35,
			Reason: ReasonInvalidMsgType,
			Msg:    fmt.Sprintf("unknown MsgType %q", msg.MsgType()),
		}
	}

	// Repeated tags and fields out of order first: QuickFIX engines find
	// them while parsing, before validating against the dictionary.
	if structure := checks & (CheckRepeatedTags | CheckOrder | CheckGroupOrder); structure != 0 {
		if err := d.checkStructure(msg, mdef, structure|checks&checksValidate); err != nil {
			return err
		}
	}

	// Presence of the tracked tags (required ones and those activating
	// optional components), anywhere and at the top level.
	var small [2][4]uint64
	present, top := small[0][:], small[1][:]
	if n := (int(mdef.slots) + 63) / 64; n > len(small[0]) {
		present, top = make([]uint64, n), make([]uint64, n)
	}

	// One pass over every field records presence and finds the first tag
	// that is not allowed or has an invalid enum value (5 and 6), reported
	// after the required-tag checks.
	var fieldErr error
	groups := false
	misplaced := 0 // the first tag allowed only in group entries and checked, wherever it is
	for _, f := range msg.Fields() {
		e := d.lookup(mdef, f.Tag)
		if e == nil {
			if fieldErr == nil && checks&unknownCheck(f.Tag) != 0 {
				fieldErr = d.unexpected(mdef, f.Tag)
			}
			if fieldErr == nil && checks&checksValue != 0 {
				fieldErr = d.checkValue(f, checks)
			}
			continue
		}
		if e.slot >= 0 {
			present[e.slot/64] |= 1 << (e.slot % 64)
		}
		groups = groups || e.counter
		if misplaced == 0 && e.grouped && checks&unknownCheck(f.Tag) != 0 {
			misplaced = f.Tag
		}
		if fieldErr == nil && !e.structural && checks&checksValue != 0 {
			fieldErr = d.checkValue(f, checks)
		}
		if fieldErr == nil && !e.structural {
			fieldErr = d.checkEnum(f)
		}
	}

	// Required tags must be present at the top level: a group entry's field
	// does not stand for a message's own, and a tag allowed only in entries
	// must not be there. Without any group, every field is.
	if groups {
		misplaced = 0
		for f := range msg.TopLevelFieldsFunc(func(tag int) *fix2.GroupSpec {
			if e := d.lookup(mdef, tag); e != nil {
				return e.spec
			}
			return nil
		}) {
			e := d.lookup(mdef, f.Tag)
			if e == nil {
				continue
			}
			if e.slot >= 0 {
				top[e.slot/64] |= 1 << (e.slot % 64)
			}
			if misplaced == 0 && e.grouped && checks&unknownCheck(f.Tag) != 0 {
				misplaced = f.Tag
			}
		}
	} else {
		copy(top, present)
	}

	// 2-4. Required header, trailer and message-body tags. Messages are
	// formatted only on error: building them would allocate on every call.
	for i, tags := range [...][]tracked{d.headerReq, d.trailerReq, mdef.requiredTags} {
		for _, t := range tags {
			if !has(top, t.slot) {
				var msg string
				switch i {
				case 0:
					msg = fmt.Sprintf("required header tag %d missing", t.tag)
				case 1:
					msg = fmt.Sprintf("required trailer tag %d missing", t.tag)
				default:
					msg = fmt.Sprintf("required tag %d missing for message %s", t.tag, mdef.Name)
				}
				return &ValidationError{Err: ErrRequiredTag, Tag: t.tag, Reason: ReasonRequiredTagMissing, Msg: msg}
			}
		}
	}

	// 4b. Required fields of optional components that are present: any of
	// their activation tags is, anywhere, since a component can show its
	// presence through one of its groups. A header or trailer activation tag
	// the message also declares in its body could be the body's.
	for i, conds := range [...][]compiledCond{d.headerConds, d.trailerConds, mdef.conds} {
		envelope := i < 2
		for _, c := range conds {
			if !slices.ContainsFunc(c.activation, func(t tracked) bool {
				if !has(present, t.slot) {
					return false
				}
				if envelope {
					if e := mdef.body.get(t.tag); e != nil && e.declared {
						return false
					}
				}
				return true
			}) {
				continue
			}
			for _, t := range c.required {
				if !has(top, t.slot) {
					where := [...]string{"header", "trailer", "message " + mdef.Name}[i]
					return &ValidationError{
						Err:    ErrRequiredTag,
						Tag:    t.tag,
						Reason: ReasonRequiredTagMissing,
						Msg:    fmt.Sprintf("required tag %d missing for component %s in %s", t.tag, c.component, where),
					}
				}
			}
		}
	}

	if groups := checks & (CheckGroupCounts | CheckGroupRequired); fieldErr == nil && groups != 0 {
		fieldErr = d.checkStructure(msg, mdef, groups)
	}
	if fieldErr == nil && misplaced != 0 {
		return &ValidationError{
			Err:    ErrTagOutsideGroup,
			Tag:    misplaced,
			Reason: ReasonTagNotDefinedForMessageType,
			Msg:    fmt.Sprintf("tag %d%s not allowed outside group entries in message %s", misplaced, d.fieldName(misplaced), mdef.Name),
		}
	}
	return fieldErr
}

// unexpected reports tag, which mdef does not allow: an invalid tag number if
// the dictionary does not define it at all, as QuickFIX and its acceptance
// tests have it (FIX's "undefined tag", reason 3, is less used), a tag not
// defined for the message type otherwise.
func (d *Dict) unexpected(mdef *MessageDef, tag int) *ValidationError {
	if fd := d.Fields[tag]; fd != nil {
		return &ValidationError{
			Err:    ErrUnexpectedTag,
			Tag:    tag,
			Reason: ReasonTagNotDefinedForMessageType,
			Msg:    fmt.Sprintf("tag %d (%s) not allowed in message %s", tag, fd.Name, mdef.Name),
		}
	}
	return &ValidationError{
		Err:    ErrUnexpectedTag,
		Tag:    tag,
		Reason: ReasonInvalidTagNumber,
		Msg:    fmt.Sprintf("tag %d not defined by the dictionary, in message %s", tag, mdef.Name),
	}
}

// unknownCheck returns the check rejecting tag when the message does not
// define it: CheckUserDefinedTags for a user-defined tag, CheckUnknownTags
// otherwise.
func unknownCheck(tag int) Checks {
	if tag >= userDefinedTagMin {
		return CheckUserDefinedTags
	}
	return CheckUnknownTags
}

// lookup returns what the message, then the envelope, knows of tag, or nil
// if the message does not allow it.
func (d *Dict) lookup(mdef *MessageDef, tag int) *tagEntry {
	if e := mdef.body.get(tag); e != nil {
		return e
	}
	return d.envelope.get(tag)
}

// has reports whether slot is set in bits.
func has(bits []uint64, slot int32) bool {
	return bits[slot/64]&(1<<(slot%64)) != 0
}

// checkEnum returns an error if f's field has enums and f's value, or one of
// its space-separated values for a multiple-value field, is not one of them.
func (d *Dict) checkEnum(f fix2.Field) error {
	e := d.enums.get(f.Tag)
	if e == nil {
		return nil
	}
	ec := &d.enumChecks[e.slot]
	if !ec.multi {
		if ec.valid(f.Value) {
			return nil
		}
		return &ValidationError{
			Err:    ErrInvalidEnumValue,
			Tag:    f.Tag,
			Reason: ReasonValueIsIncorrect,
			Msg:    fmt.Sprintf("invalid value %q for tag %d (%s)", f.Value, f.Tag, ec.name),
		}
	}
	for item := range bytes.SplitSeq(f.Value, space) {
		if !ec.valid(item) {
			return &ValidationError{
				Err:    ErrInvalidEnumValue,
				Tag:    f.Tag,
				Reason: ReasonValueIsIncorrect,
				Msg:    fmt.Sprintf("invalid value %q in %q for tag %d (%s)", item, f.Value, f.Tag, ec.name),
			}
		}
	}
	return nil
}

var space = []byte{' '}

// isMultipleValue reports whether fixType holds space-separated values:
// MULTIPLEVALUESTRING in FIX 4.2-4.4, MULTIPLECHARVALUE and
// MULTIPLESTRINGVALUE from FIX 4.4/5.0 on.
func isMultipleValue(fixType string) bool {
	switch fixType {
	case "MULTIPLEVALUESTRING", "MULTIPLECHARVALUE", "MULTIPLESTRINGVALUE":
		return true
	}
	return false
}

// groupSpecs returns the specs of groups, whose entries keep the tags
// unknown reports. An entry may also hold the LENGTH field of its DATA
// fields, which the parser requires right before them, and the DATA field of
// its LENGTH fields: generated builders write both, even when the dictionary
// declares only one.
func (d *Dict) groupSpecs(groups []GroupDef, unknown func(int) bool) []*fix2.GroupSpec {
	specs := make([]*fix2.GroupSpec, 0, len(groups))
	for _, gd := range groups {
		s := &fix2.GroupSpec{Counter: gd.CounterTag, Delim: gd.DelimTag, Unknown: unknown}
		// LENGTH comes right before its DATA field, declared or not, so an
		// entry whose first field is DATA starts with LENGTH.
		if l := d.profile.DataLengthTag(gd.DelimTag); l != 0 {
			s.Delim = l
		}
		added := make(map[int]bool, len(gd.Fields))
		add := func(tag int) {
			if !added[tag] {
				added[tag] = true
				s.Tags = append(s.Tags, tag)
			}
		}
		for _, fr := range gd.Fields {
			if l := d.profile.DataLengthTag(fr.Tag); l != 0 {
				add(l)
			}
			add(fr.Tag)
			if dt := d.profile.DataTag(fr.Tag); dt != 0 {
				add(dt)
			}
		}
		s.Nested = d.groupSpecs(gd.Groups, unknown)
		d.groupRules[s] = d.rulesOf(gd, s)
		specs = append(specs, s)
	}
	return specs
}

// groupRules is what an entry of a group requires (see CheckGroupRequired
// and CheckGroupOrder), its tags by position (see position).
type groupRules struct {
	name     string
	rank     []int32 // a tag's place in the group's Layout, -1 if none
	required []int
	conds    []entryCond
	// activates holds, by position, the indexes in conds of the conditions
	// its tag activates; nil without conditions.
	activates [][]int
	// pos maps a tag to its position, as its entry's slot, for groups of
	// at least indexedGroup tags and nested groups; empty for narrower ones,
	// which position scans.
	pos tagTable
}

// indexedGroup is the number of tags and nested groups from which position
// looks a tag up in groupRules.pos rather than scanning, as fix2.GroupSpec
// does: below it, scanning is as fast, and a wide group then costs no more
// per field than a narrow one.
const indexedGroup = 16

// entryCond is a Condition of a group's entries, its tags by position.
type entryCond struct {
	component string
	required  []int
}

// rulesOf returns the rules of the entries of gd, of spec s.
func (d *Dict) rulesOf(gd GroupDef, s *fix2.GroupSpec) *groupRules {
	n := len(s.Tags) + len(s.Nested)
	r := &groupRules{name: gd.Name, rank: make([]int32, n)}
	// Positions by tag, the first winning and a nested counter's over a
	// tag's, as in position.
	pos := make(map[int]tagEntry, n)
	for k := len(s.Tags) - 1; k >= 0; k-- {
		pos[s.Tags[k]] = tagEntry{slot: int32(k)}
	}
	for k := n - 1; k >= len(s.Tags); k-- {
		pos[tagAt(s, k)] = tagEntry{slot: int32(k)}
	}
	if n >= indexedGroup {
		r.pos = newTagTable(pos)
	}
	at := func(tag int) int {
		if e, ok := pos[tag]; ok {
			return int(e.slot)
		}
		return -1
	}
	layout := make(map[int]int32, len(gd.Layout))
	for i, tag := range gd.Layout {
		if _, ok := layout[tag]; !ok {
			layout[tag] = int32(i)
		}
	}
	for k := range r.rank {
		tag := tagAt(s, k)
		// A LENGTH field and its DATA field, one right after the other,
		// sit where the group declares the first of them.
		rank := int32(-1)
		for _, t := range [...]int{tag, d.profile.DataTag(tag), d.profile.DataLengthTag(tag)} {
			if i, ok := layout[t]; ok && t != 0 && (rank < 0 || i < rank) {
				rank = i
			}
		}
		r.rank[k] = rank
	}
	required := make([]bool, n)
	for _, ref := range gd.Fields {
		if ref.Required {
			if k := at(ref.Tag); k >= 0 {
				required[k] = true
			}
		}
	}
	for _, g := range gd.Groups {
		if g.Required {
			if k := at(g.CounterTag); k >= 0 {
				required[k] = true
			}
		}
	}
	for k, req := range required {
		if req {
			r.required = append(r.required, k)
		}
	}
	// Activation tags held by nested entries only cannot be told from the
	// entry's own: the nested group's counter, the entry's, stands for them.
	declared := once(func() map[int]bool { return groupTags(gd) })
	for _, c := range gd.Conditions {
		ec := entryCond{component: c.Component}
		var activation []int
		for _, tag := range d.activation(c.Activation, declared) {
			if k := at(tag); k >= 0 {
				activation = append(activation, k)
			}
		}
		for _, tag := range c.Required {
			if k := at(tag); k >= 0 {
				ec.required = append(ec.required, k)
			}
		}
		if len(activation) == 0 || len(ec.required) == 0 {
			continue
		}
		// An entry finds the conditions it activates from the tags it
		// holds, not from every activation tag: a wide optional component
		// then costs nothing to an entry without it.
		if r.activates == nil {
			r.activates = make([][]int, n)
		}
		for _, k := range activation {
			if as := r.activates[k]; len(as) == 0 || as[len(as)-1] != len(r.conds) {
				r.activates[k] = append(as, len(r.conds))
			}
		}
		r.conds = append(r.conds, ec)
	}
	return r
}

// activation returns the tags of a condition's activation that show its
// component is present: not a LENGTH or DATA field whose partner the scope
// declares outside the component, which may bring it along. declared
// returns the tags the scope declares; it is called only if needed.
func (d *Dict) activation(tags []int, declared func() map[int]bool) []int {
	var in, outside map[int]bool
	var out []int // nil while every tag is kept
	for i, tag := range tags {
		keep := true
		if dt, lt := d.profile.DataTag(tag), d.profile.DataLengthTag(tag); dt != 0 || lt != 0 {
			if in == nil {
				in = make(map[int]bool, len(tags))
				for _, t := range tags {
					in[t] = true
				}
				outside = declared()
			}
			keep = (dt == 0 || in[dt] || !outside[dt]) && (lt == 0 || in[lt] || !outside[lt])
		}
		switch {
		case !keep && out == nil:
			out = append(make([]int, 0, len(tags)), tags[:i]...)
		case keep && out != nil:
			out = append(out, tag)
		}
	}
	if out == nil {
		return tags
	}
	return out
}

// once returns a function returning what f returns, calling f once.
func once(f func() map[int]bool) func() map[int]bool {
	var m map[int]bool
	return func() map[int]bool {
		if m == nil {
			m = f()
		}
		return m
	}
}

// refTags returns the tags of refs.
func refTags(refs ...[]FieldRef) map[int]bool {
	tags := map[int]bool{}
	for _, rs := range refs {
		for _, ref := range rs {
			tags[ref.Tag] = true
		}
	}
	return tags
}

// groupTags returns the tags an entry of gd may hold, its nested entries'
// included.
func groupTags(gd GroupDef) map[int]bool {
	tags := refTags(gd.Fields)
	var walk func([]GroupDef)
	walk = func(groups []GroupDef) {
		for _, g := range groups {
			tags[g.CounterTag] = true
			for _, ref := range g.Fields {
				tags[ref.Tag] = true
			}
			walk(g.Groups)
		}
	}
	walk(gd.Groups)
	return tags
}

// tagAt returns the tag at position k of spec (see position).
func tagAt(spec *fix2.GroupSpec, k int) int {
	if k < len(spec.Tags) {
		return spec.Tags[k]
	}
	return spec.Nested[k-len(spec.Tags)].Counter
}
