// Package xmldict reads FIX data dictionaries from QuickFIX XML. fixgen uses
// it to generate packages, whose dictionaries need no XML at run time; a
// program that only learns its dictionary at run time can load it here.
package xmldict

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/sylr/fix2/dictionary"
)

// ---------------------------------------------------------------------------
// XML unmarshalling types (internal)
// ---------------------------------------------------------------------------

type xmlDict struct {
	Type        string        `xml:"type,attr"`
	Major       string        `xml:"major,attr"`
	Minor       string        `xml:"minor,attr"`
	ServicePack string        `xml:"servicepack,attr"`
	Header      xmlBody       `xml:"header"`
	Trailer     xmlBody       `xml:"trailer"`
	Messages    xmlMessages   `xml:"messages"`
	Components  xmlComponents `xml:"components"`
	Fields      xmlFieldDefs  `xml:"fields"`
}

type xmlMessages struct {
	Messages []xmlMessage `xml:"message"`
}

type xmlComponents struct {
	Components []xmlComponent `xml:"component"`
}

type xmlFieldDefs struct {
	Fields []xmlFieldDef `xml:"field"`
}

// xmlBody is the content of a header, trailer, message, component or group.
// Children are kept in a single slice so their XML order is preserved: a
// group's first child, possibly inside a component, is its delimiter.
type xmlBody struct {
	Items []xmlItem `xml:",any"`
}

// xmlItem is a <field>, <component> or <group> reference, told apart by
// XMLName. Only groups have children.
type xmlItem struct {
	XMLName  xml.Name
	Name     string `xml:"name,attr"`
	Required string `xml:"required,attr"`
	xmlBody
}

type xmlMessage struct {
	Name    string `xml:"name,attr"`
	MsgType string `xml:"msgtype,attr"`
	MsgCat  string `xml:"msgcat,attr"`
	xmlBody
}

type xmlComponent struct {
	Name string `xml:"name,attr"`
	xmlBody
}

type xmlFieldDef struct {
	Number int            `xml:"number,attr"`
	Name   string         `xml:"name,attr"`
	Type   string         `xml:"type,attr"`
	Values []xmlEnumValue `xml:"value"`
}

type xmlEnumValue struct {
	Enum        string `xml:"enum,attr"`
	Description string `xml:"description,attr"`
}

// rawCondition is a condition while its body is being resolved.
type rawCondition struct {
	component string
	parent    int         // index of the enclosing optional component, or -1
	counts    map[int]int // occurrences of each tag inside the component
	required  []int
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// maxDictSize bounds the XML Load reads, and so the memory decoding can use
// before any resolver limit applies. The full FIX 5.0 SP2 dictionary is about
// 1.5 MB.
const maxDictSize = 64 << 20

// Load parses a QuickFIX XML dictionary from r. It reads at most 64 MiB,
// counting what the XML decoder buffers ahead, and at most 262,144 elements.
func Load(r io.Reader) (*dictionary.Dict, error) {
	lr := &io.LimitedReader{R: r, N: maxDictSize + 1}
	xd, err := decode(lr)
	// Reading the extra byte means the input is over the limit, whether or
	// not the decoder managed to finish.
	if lr.N <= 0 {
		return nil, fmt.Errorf("dictionary: larger than %d bytes", maxDictSize)
	}
	if err != nil {
		return nil, fmt.Errorf("dictionary: %w", err)
	}
	return resolve(xd)
}

// LoadFile parses a QuickFIX XML dictionary from the file at path.
func LoadFile(path string) (*dictionary.Dict, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dictionary: %w", err)
	}
	defer f.Close()
	return Load(f)
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func resolve(xd *xmlDict) (*dictionary.Dict, error) {
	d := &dictionary.Dict{
		Type:     xd.Type,
		Major:    xd.Major,
		Minor:    xd.Minor,
		Fields:   make(map[int]*dictionary.FieldDef, len(xd.Fields.Fields)),
		Messages: make(map[string]*dictionary.MessageDef, len(xd.Messages.Messages)),
	}

	// 1. Build Fields map and the index by name references are resolved with.
	byName := make(map[string]*dictionary.FieldDef, len(xd.Fields.Fields))
	for _, xf := range xd.Fields.Fields {
		if err := checkName("field", xf.Name); err != nil {
			return nil, fmt.Errorf("dictionary: %w", err)
		}
		if prev, ok := d.Fields[xf.Number]; ok {
			return nil, fmt.Errorf("dictionary: fields %s and %s have the same number %d", prev.Name, xf.Name, xf.Number)
		}
		if prev, ok := byName[xf.Name]; ok {
			return nil, fmt.Errorf("dictionary: fields %d and %d have the same name %s", prev.Tag, xf.Number, xf.Name)
		}
		fd := &dictionary.FieldDef{
			Tag:  xf.Number,
			Name: xf.Name,
			Type: xf.Type,
		}
		if len(xf.Values) > 0 {
			fd.Enums = make(map[string]string, len(xf.Values))
			for _, v := range xf.Values {
				fd.Enums[v.Enum] = v.Description
			}
		}
		d.Fields[xf.Number] = fd
		byName[xf.Name] = fd
	}

	// 2. Build component map.
	compMap := make(map[string]*xmlComponent, len(xd.Components.Components))
	for i := range xd.Components.Components {
		c := &xd.Components.Components[i]
		if err := checkName("component", c.Name); err != nil {
			return nil, fmt.Errorf("dictionary: %w", err)
		}
		compMap[c.Name] = c
	}

	r := &resolver{byName: byName, compMap: compMap, visiting: map[string]bool{}}

	// 3. Flatten header.
	header, err := r.resolve(xd.Header.Items)
	if err != nil {
		return nil, fmt.Errorf("dictionary: header: %w", err)
	}
	d.Header = header.all
	d.HeaderGroups = header.groups

	// 4. Flatten trailer.
	trailer, err := r.resolve(xd.Trailer.Items)
	if err != nil {
		return nil, fmt.Errorf("dictionary: trailer: %w", err)
	}
	d.Trailer = trailer.all
	d.TrailerGroups = trailer.groups

	// Tag occurrences in the header and trailer, which every message carries.
	envelope := map[int]int{}
	countTags(envelope, header.all)
	countTags(envelope, trailer.all)

	// 5. Flatten each message and resolve its groups.
	inMessages := map[int]bool{}
	for _, xm := range xd.Messages.Messages {
		body, err := r.resolve(xm.Items)
		if err != nil {
			return nil, fmt.Errorf("dictionary: message %s: %w", xm.Name, err)
		}
		// Count the body on its own: cloning the envelope counts for every
		// message would be uncharged work proportional to header size times
		// message count.
		inBody := map[int]int{}
		countTags(inBody, body.all)
		for _, ref := range body.all {
			inMessages[ref.Tag] = true
		}
		d.Messages[xm.MsgType] = &dictionary.MessageDef{
			Name:       xm.Name,
			MsgType:    xm.MsgType,
			MsgCat:     xm.MsgCat,
			Fields:     body.all,
			Groups:     body.groups,
			Conditions: body.conditions(envelope, inBody, nil),
		}
	}

	// Header and trailer conditions apply to every message, so a tag also used
	// in any message body cannot tell their components apart.
	d.HeaderConditions = header.conditions(envelope, nil, inMessages)
	d.TrailerConditions = trailer.conditions(envelope, nil, inMessages)

	return dictionary.New(d), nil
}

// countTags adds one occurrence per ref to counts.
func countTags(counts map[int]int, refs []dictionary.FieldRef) {
	for _, ref := range refs {
		counts[ref.Tag]++
	}
}

// resolver expands field, component and group references into FieldRefs and
// GroupDefs. It walks items in XML order and expands components in place, so
// fields and groups keep their position at any depth.
type resolver struct {
	byName   map[string]*dictionary.FieldDef
	compMap  map[string]*xmlComponent
	visiting map[string]bool // components on the current path, for cycle detection
	visits   int             // items visited plus group refs copied, bounded by maxVisits
	depth    int             // current component/group nesting, bounded by maxDepth
}

// maxVisits bounds the items (fields, groups and component references) the
// resolver visits for a whole dictionary. Components are expanded in place,
// so a small acyclic dictionary where each component references the previous
// one twice expands to 2^n items; cycle detection does not catch that.
// Component references count too, so a chain of empty components is bounded
// as well.
const maxVisits = 1_000_000

// maxDepth bounds component and group nesting. It keeps the recursion, and
// the chain of wrapped errors built while unwinding it, small. The deepest
// nesting in the full FIX 5.0 SP2 dictionary is 11.
const maxDepth = 64

// maxNameLen bounds field, component and group names. Each reference costs
// one visit but hashes its name for a map lookup, so long names would make
// the visit budget meaningless. The longest standard FIX name has 38 bytes.
const maxNameLen = 256

// checkName rejects names longer than maxNameLen.
func checkName(kind, name string) error {
	if len(name) > maxNameLen {
		return fmt.Errorf("%s name of %d bytes exceeds %d", kind, len(name), maxNameLen)
	}
	return nil
}

// spend charges n units of work against maxVisits.
func (r *resolver) spend(n int) error {
	if r.visits += n; r.visits > maxVisits {
		return fmt.Errorf("expansion exceeds %d items: components are referenced too many times", maxVisits)
	}
	return nil
}

// enter descends one component or group level; the caller must call leave.
func (r *resolver) enter() error {
	if r.depth++; r.depth > maxDepth {
		return fmt.Errorf("components and groups nested deeper than %d levels", maxDepth)
	}
	return nil
}

func (r *resolver) leave() { r.depth-- }

// resolvedBody is the resolved content of a header, trailer, message or group.
type resolvedBody struct {
	all    []dictionary.FieldRef // every tag the body can hold, group contents included
	direct []dictionary.FieldRef // fields outside any group, components expanded
	groups []dictionary.GroupDef // groups at this level, components expanded
	first  int                   // first tag in XML order (field or group counter); 0 if none
	layout []int                 // fields and group counters at this level, in XML order
	conds  []rawCondition
}

// markPresent records tag as part of the optional component at index ctx and
// of every optional component enclosing it.
func (b *resolvedBody) markPresent(ctx, tag int) {
	for i := ctx; i >= 0; i = b.conds[i].parent {
		b.conds[i].counts[tag]++
	}
}

// condDepth returns how many optional components enclose ctx, i.e. how many
// map updates markPresent does per tag. The resolver charges it before
// calling markPresent.
func (b *resolvedBody) condDepth(ctx int) int {
	n := 0
	for i := ctx; i >= 0; i = b.conds[i].parent {
		n++
	}
	return n
}

// addField records a field or group counter seen in the optional component at
// index ctx (-1 outside any) and returns its ref. A required field is
// required unconditionally only outside optional components; inside one it
// becomes a requirement of that component.
func (b *resolvedBody) addField(ctx, tag int, required bool) dictionary.FieldRef {
	b.markPresent(ctx, tag)
	if required && ctx >= 0 {
		b.conds[ctx].required = append(b.conds[ctx].required, tag)
	}
	return dictionary.FieldRef{Tag: tag, Required: required && ctx < 0}
}

// conditions turns the optional components that have required fields into
// conditions. A tag activates a component only if every occurrence of it,
// counted in the envelope (header and trailer) plus body, lies inside the
// component and it is not in exclude; otherwise its presence could come from
// elsewhere. A component left without activation tags is dropped: its
// presence cannot be told apart, and enforcing it could reject valid messages.
func (b *resolvedBody) conditions(envelope, body map[int]int, exclude map[int]bool) []dictionary.Condition {
	var out []dictionary.Condition
	for _, c := range b.conds {
		if len(c.required) == 0 {
			continue
		}
		var activation []int
		for tag, n := range c.counts {
			if n == envelope[tag]+body[tag] && !exclude[tag] {
				activation = append(activation, tag)
			}
		}
		if len(activation) == 0 {
			continue
		}
		slices.Sort(activation)
		out = append(out, dictionary.Condition{Component: c.component, Activation: activation, Required: c.required})
	}
	return out
}

func (b *resolvedBody) setFirst(tag int) {
	if b.first == 0 {
		b.first = tag
	}
}

func (r *resolver) resolve(items []xmlItem) (resolvedBody, error) {
	var b resolvedBody
	err := r.walk(items, &b, -1)
	return b, err
}

// walk resolves items into b. ctx is the index in b.conds of the innermost
// optional component being expanded, or -1 outside any.
func (r *resolver) walk(items []xmlItem, b *resolvedBody, ctx int) error {
	for _, it := range items {
		if err := r.spend(1); err != nil {
			return err
		}
		// Check before hashing it for a lookup: a visit costs the same
		// whatever the name length.
		if err := checkName(it.XMLName.Local, it.Name); err != nil {
			return err
		}
		switch it.XMLName.Local {
		case "field":
			fd, ok := r.byName[it.Name]
			if !ok {
				return fmt.Errorf("unknown field %q", it.Name)
			}
			if err := r.spend(b.condDepth(ctx)); err != nil {
				return err
			}
			ref := b.addField(ctx, fd.Tag, it.Required == "Y")
			b.all = append(b.all, ref)
			b.direct = append(b.direct, ref)
			b.layout = append(b.layout, fd.Tag)
			b.setFirst(fd.Tag)

		case "component":
			comp, ok := r.compMap[it.Name]
			if !ok {
				return fmt.Errorf("unknown component %q", it.Name)
			}
			if r.visiting[it.Name] {
				return fmt.Errorf("cycle detected in component %q", it.Name)
			}
			// A required component keeps the enclosing context; an optional
			// one opens a new condition nested in it.
			child := ctx
			if it.Required != "Y" {
				b.conds = append(b.conds, rawCondition{component: it.Name, parent: ctx, counts: map[int]int{}})
				child = len(b.conds) - 1
			}
			if err := r.enter(); err != nil {
				return err
			}
			r.visiting[it.Name] = true
			err := r.walk(comp.Items, b, child)
			delete(r.visiting, it.Name)
			r.leave()
			if err != nil {
				return fmt.Errorf("component %s: %w", it.Name, err)
			}

		case "group":
			// The group name is also a field name (the counter tag).
			fd, ok := r.byName[it.Name]
			if !ok {
				return fmt.Errorf("unknown group field %q", it.Name)
			}
			if err := r.enter(); err != nil {
				return err
			}
			inner, err := r.resolve(it.Items)
			r.leave()
			if err != nil {
				return fmt.Errorf("group %s: %w", it.Name, err)
			}
			// The group's contents are copied into this body: charge them, or
			// a chain of nested groups would do quadratic uncharged work.
			if err := r.spend(len(inner.all)); err != nil {
				return err
			}
			if inner.first == 0 {
				return fmt.Errorf("group %s: no child fields", it.Name)
			}
			// Charge recording the counter and every group child in the
			// enclosing optional components before doing it.
			if err := r.spend(b.condDepth(ctx) * (1 + len(inner.all))); err != nil {
				return err
			}
			b.all = append(b.all, b.addField(ctx, fd.Tag, it.Required == "Y"))
			// Group children are never required at this level since group
			// instance validation is out of scope. They do show that their
			// optional component is present.
			for _, ref := range inner.all {
				b.markPresent(ctx, ref.Tag)
				b.all = append(b.all, dictionary.FieldRef{Tag: ref.Tag})
			}
			b.groups = append(b.groups, dictionary.GroupDef{
				CounterTag: fd.Tag,
				Name:       it.Name,
				Required:   it.Required == "Y",
				DelimTag:   inner.first,
				Fields:     inner.direct,
				Groups:     inner.groups,
				Layout:     inner.layout,
			})
			b.layout = append(b.layout, fd.Tag)
			b.setFirst(fd.Tag)

		default:
			return fmt.Errorf("unexpected element <%s>", it.XMLName.Local)
		}
	}
	return nil
}
