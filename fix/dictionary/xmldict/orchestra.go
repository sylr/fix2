package xmldict

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// orchestraNS is the namespace of Orchestra 1.0 repositories, the format the
// FIX Trading Community publishes FIX Latest, FIX 4.2, FIX 4.4 and the FIXT
// session layer in.
const orchestraNS = "http://fixprotocol.io/2020/orchestra/repository"

// maxOrchestraEnumValues bounds the enum values the fields of an Orchestra
// repository get from their code sets. A code set can type any number of
// fields, so codes do not bound them as QuickFIX <value> elements do. FIX
// Latest as of Extension Pack 312 has about 27,000.
const maxOrchestraEnumValues = 1 << 18

// isOrchestra tells whether root is the root element of an Orchestra
// repository. It returns an error for the root of a repository in another
// FIX namespace, such as the draft of 2016, rather than read it as a
// QuickFIX dictionary.
func isOrchestra(root xml.StartElement) (bool, error) {
	if root.Name.Local != "repository" || !strings.HasPrefix(root.Name.Space, "http://fixprotocol.io/") {
		return false, nil
	}
	if root.Name.Space != orchestraNS {
		return false, fmt.Errorf("Orchestra namespace %q is not supported, only %q", root.Name.Space, orchestraNS)
	}
	return true, nil
}

// orchRef is a <fieldRef>, <componentRef> or <groupRef>: kind is the element
// name, id what it references.
type orchRef struct {
	kind     string
	id       int
	scenario string
	presence string
}

// orchStruct is a <component> or <group>, or a <message>'s <structure>. Only
// groups have a counter, the id of their NumInGroup field.
type orchStruct struct {
	id         int
	name       string
	scenario   string
	counter    int
	hasCounter bool
	refs       []orchRef
}

type orchMessage struct {
	name, msgType, category, scenario string
	refs                              []orchRef
}

// orchField is a <field>: lengthID is the id of the length field of a data
// field, or 0.
type orchField struct {
	id, lengthID        int
	name, typ, scenario string
}

type orchCodeSet struct {
	name, typ, scenario string
	codes               []xmlEnumValue
	deprecated          []bool // by code
}

// orchestra reads the rest of an Orchestra repository whose root is root,
// and translates it into the QuickFIX model:
//
//   - the length field a data field names with lengthId is a LENGTH, if
//     typed int;
//   - a field whose type is a code set takes the code set's datatype and
//     codes, a code's name becoming its description in upper snake case, as
//     in QuickFIX (PartiallyFilled becomes PARTIALLY_FILLED), with a
//     _DEPRECATED suffix if it is deprecated and another code of the set
//     has the same;
//   - datatypes are upper cased (UTCTimestamp becomes UTCTIMESTAMP);
//   - the StandardHeader and StandardTrailer components are the header and
//     trailer, and messages' references to them are dropped;
//   - groups are named after their NumInGroup field;
//   - messages of the Session category are admin messages, the others app;
//     without a category, as in FIX 4.2, the session messages are admin;
//   - required references are required, forbidden ones dropped, and all
//     others optional;
//   - definitions of scenarios other than the base one are ignored.
//
// The version comes from the repository's version attribute, or its name:
// "FIX.4.4", "FIX.5.0SP2_EP254" or "FIX.Latest_EP312", whose major version is
// "Latest". A repository named FIXT is FIXT 1.1.
func (dec *decoder) orchestra(root xml.StartElement) (*xmlDict, error) {
	var (
		codeSets   []orchCodeSet
		fields     []orchField
		components []orchStruct
		groups     []orchStruct
		messages   []orchMessage
	)
	err := dec.children(func(start xml.StartElement) error {
		switch start.Name.Local {
		case "codeSets":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "codeSet" {
					return dec.skip()
				}
				cs := orchCodeSet{name: attr(start, "name"), typ: attr(start, "type"), scenario: attr(start, "scenario")}
				err := dec.children(func(start xml.StartElement) error {
					if start.Name.Local == "code" {
						cs.codes = append(cs.codes, xmlEnumValue{Enum: attr(start, "value"), Description: enumDescription(attr(start, "name"))})
						cs.deprecated = append(cs.deprecated, attr(start, "deprecated") != "")
					}
					return dec.skip()
				})
				cs.markDeprecated()
				codeSets = append(codeSets, cs)
				return err
			})
		case "fields":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "field" {
					return dec.skip()
				}
				f := orchField{name: attr(start, "name"), typ: attr(start, "type"), scenario: attr(start, "scenario")}
				id, err := idAttr(start)
				if err != nil {
					return fmt.Errorf("field %q: %w", f.name, err)
				}
				f.id = id
				if v := attr(start, "lengthId"); v != "" {
					if f.lengthID, err = parseID(v); err != nil {
						return fmt.Errorf("field %q: lengthId: %w", f.name, err)
					}
				}
				fields = append(fields, f)
				return dec.skip()
			})
		case "components", "groups":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "component" && start.Name.Local != "group" {
					return dec.skip()
				}
				s := orchStruct{name: attr(start, "name"), scenario: attr(start, "scenario")}
				id, err := idAttr(start)
				if err != nil {
					return fmt.Errorf("%s %q: %w", start.Name.Local, s.name, err)
				}
				s.id = id
				if err := dec.refs(&s); err != nil {
					return fmt.Errorf("%s %q: %w", start.Name.Local, s.name, err)
				}
				if start.Name.Local == "group" {
					groups = append(groups, s)
				} else {
					components = append(components, s)
				}
				return nil
			})
		case "messages":
			return dec.children(func(start xml.StartElement) error {
				if start.Name.Local != "message" {
					return dec.skip()
				}
				m := orchMessage{name: attr(start, "name"), msgType: attr(start, "msgType"), category: attr(start, "category"), scenario: attr(start, "scenario")}
				err := dec.children(func(start xml.StartElement) error {
					if start.Name.Local != "structure" {
						return dec.skip()
					}
					var s orchStruct
					err := dec.refs(&s)
					m.refs = append(m.refs, s.refs...)
					return err
				})
				if err != nil {
					return fmt.Errorf("message %q: %w", m.name, err)
				}
				messages = append(messages, m)
				return nil
			})
		}
		return dec.skip()
	})
	if err != nil {
		return nil, err
	}

	xd := &xmlDict{orchestra: true}
	name, version := attr(root, "name"), attr(root, "version")
	var ok bool
	if xd.Type, xd.Major, xd.Minor, xd.ServicePack, ok = orchestraVersion(name, version); !ok {
		return nil, fmt.Errorf("repository %q: no FIX version in version %q", name, version)
	}

	// Code sets by name, fields by id.
	byName := map[string]*orchCodeSet{}
	for i := range codeSets {
		if cs := &codeSets[i]; isBase(cs.scenario) {
			byName[cs.name] = cs
		}
	}
	fieldNames := map[int]string{}
	fieldIndex := map[int]int{} // index in xd.Fields.Fields, by id
	enumValues := 0
	for _, f := range fields {
		if !isBase(f.scenario) {
			continue
		}
		// resolve reports fields sharing an id.
		if _, ok := fieldNames[f.id]; !ok {
			fieldNames[f.id], fieldIndex[f.id] = f.name, len(xd.Fields.Fields)
		}
		fd := xmlFieldDef{Number: f.id, Name: f.name, Type: strings.ToUpper(f.typ)}
		if cs := byName[f.typ]; cs != nil {
			if enumValues += len(cs.codes); enumValues > maxOrchestraEnumValues {
				return nil, fmt.Errorf("fields have more than %d enum values: code sets are used too many times", maxOrchestraEnumValues)
			}
			fd.Type = strings.ToUpper(cs.typ)
			fd.Values = cs.codes
		}
		xd.Fields.Fields = append(xd.Fields.Fields, fd)
	}
	// The length field of a data field is a LENGTH, which pairs them for
	// parsing (see dictionary.Dict.Profile): FIX 4.2 types it int.
	for _, f := range fields {
		if i, ok := fieldIndex[f.lengthID]; ok && isBase(f.scenario) && xd.Fields.Fields[i].Type == "INT" {
			xd.Fields.Fields[i].Type = "LENGTH"
		}
	}

	// Components and groups by id. The resolver finds them by name: names
	// must be unique too.
	byID := func(kind string, list []orchStruct) (map[int]*orchStruct, error) {
		m := map[int]*orchStruct{}
		names := map[string]int{}
		for i := range list {
			s := &list[i]
			if !isBase(s.scenario) {
				continue
			}
			if prev := m[s.id]; prev != nil {
				return nil, fmt.Errorf("%ss %s and %s have the same id %d", kind, prev.name, s.name, s.id)
			}
			if id, ok := names[s.name]; ok {
				return nil, fmt.Errorf("%ss %d and %d have the same name %s", kind, id, s.id, s.name)
			}
			m[s.id], names[s.name] = s, s.id
		}
		return m, nil
	}
	componentsByID, err := byID("component", components)
	if err != nil {
		return nil, err
	}
	groupsByID, err := byID("group", groups)
	if err != nil {
		return nil, err
	}

	// items translates refs into the items of a QuickFIX body.
	items := func(refs []orchRef) ([]xmlItem, error) {
		var out []xmlItem
		for _, ref := range refs {
			if ref.presence == "forbidden" {
				continue
			}
			if !isBase(ref.scenario) {
				return nil, fmt.Errorf("%s %d: scenario %q is not supported", ref.kind, ref.id, ref.scenario)
			}
			it := xmlItem{}
			if ref.presence == "required" {
				it.Required = "Y"
			}
			switch ref.kind {
			case "fieldRef":
				name, ok := fieldNames[ref.id]
				if !ok {
					return nil, fmt.Errorf("unknown field id %d", ref.id)
				}
				it.XMLName.Local, it.Name = "field", name
			case "componentRef":
				c := componentsByID[ref.id]
				if c == nil {
					return nil, fmt.Errorf("unknown component id %d", ref.id)
				}
				if isEnvelope(c.name) {
					continue
				}
				it.XMLName.Local, it.Name = "component", c.name
			case "groupRef":
				g := groupsByID[ref.id]
				if g == nil {
					return nil, fmt.Errorf("unknown group id %d", ref.id)
				}
				it.XMLName.Local, it.Name = "groupRef", g.name
			}
			out = append(out, it)
		}
		return out, nil
	}

	for _, c := range components {
		if !isBase(c.scenario) {
			continue
		}
		body, err := items(c.refs)
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", c.name, err)
		}
		switch c.name {
		case "StandardHeader":
			xd.Header.Items = body
		case "StandardTrailer":
			xd.Trailer.Items = body
		default:
			xd.Components.Components = append(xd.Components.Components, xmlComponent{Name: c.name, xmlBody: xmlBody{Items: body}})
		}
	}
	for _, g := range groups {
		if !isBase(g.scenario) {
			continue
		}
		if !g.hasCounter {
			return nil, fmt.Errorf("group %s: no numInGroup", g.name)
		}
		counter, ok := fieldNames[g.counter]
		if !ok {
			return nil, fmt.Errorf("group %s: unknown NumInGroup field id %d", g.name, g.counter)
		}
		body, err := items(g.refs)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g.name, err)
		}
		xd.Groups = append(xd.Groups, xmlGroup{Name: g.name, Counter: counter, xmlBody: xmlBody{Items: body}})
	}
	seen := map[string]string{}
	for _, m := range messages {
		if !isBase(m.scenario) {
			continue
		}
		if prev, ok := seen[m.msgType]; ok {
			return nil, fmt.Errorf("messages %s and %s have the same MsgType %q", prev, m.name, m.msgType)
		}
		seen[m.msgType] = m.name
		body, err := items(m.refs)
		if err != nil {
			return nil, fmt.Errorf("message %s: %w", m.name, err)
		}
		xd.Messages.Messages = append(xd.Messages.Messages, xmlMessage{Name: m.name, MsgType: m.msgType, MsgCat: m.msgCat(), xmlBody: xmlBody{Items: body}})
	}
	return xd, nil
}

// msgCat returns the QuickFIX category of m: admin for the Session category,
// or without a category, as in FIX 4.2, for the session messages.
func (m *orchMessage) msgCat() string {
	switch {
	case m.category == "Session":
		return "admin"
	case m.category == "" && len(m.msgType) == 1 && strings.Contains("012345A", m.msgType):
		return "admin"
	}
	return "app"
}

// markDeprecated suffixes with _DEPRECATED the description of a deprecated
// code that another code of the set shares: FIX Latest replaced Euribor by
// EURIBOR in BenchmarkCurveName (221).
func (cs *orchCodeSet) markDeprecated() {
	count := make(map[string]int, len(cs.codes))
	for _, c := range cs.codes {
		count[c.Description]++
	}
	for i := range cs.codes {
		if c := &cs.codes[i]; cs.deprecated[i] && count[c.Description] > 1 {
			c.Description += "_DEPRECATED"
		}
	}
}

// refs reads the references of a component, group or message structure into
// s, and a group's NumInGroup. What references hold, such as annotations or
// rules, is skipped.
func (dec *decoder) refs(s *orchStruct) error {
	return dec.children(func(start xml.StartElement) error {
		switch start.Name.Local {
		case "fieldRef", "componentRef", "groupRef", "numInGroup":
			id, err := idAttr(start)
			if err != nil {
				return fmt.Errorf("%s: %w", start.Name.Local, err)
			}
			if start.Name.Local == "numInGroup" {
				s.counter, s.hasCounter = id, true
			} else {
				s.refs = append(s.refs, orchRef{kind: start.Name.Local, id: id, scenario: attr(start, "scenario"), presence: attr(start, "presence")})
			}
		}
		return dec.skip()
	})
}

// idAttr returns the value of start's id attribute, which is required.
func idAttr(start xml.StartElement) (int, error) {
	v := attr(start, "id")
	if v == "" {
		return 0, fmt.Errorf("no id")
	}
	n, err := parseID(v)
	if err != nil {
		return 0, fmt.Errorf("id: %w", err)
	}
	return n, nil
}

// parseID parses an id, as fieldDef parses QuickFIX field numbers.
func parseID(v string) (int, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, strconv.IntSize)
	return int(n), err
}

// isEnvelope tells whether the component named name is the standard header
// or trailer.
func isEnvelope(name string) bool { return name == "StandardHeader" || name == "StandardTrailer" }

// isBase tells whether scenario is the base scenario, the one of an element
// without a scenario attribute.
func isBase(scenario string) bool { return scenario == "" || scenario == "base" }

// orchestraVersion returns the FIX version of a repository from its version
// attribute, or failing that its name.
func orchestraVersion(name, version string) (typ, major, minor, servicePack string, ok bool) {
	if name == "FIXT" {
		return "FIXT", "1", "1", "", true
	}
	for _, s := range []string{version, name} {
		if typ, major, minor, servicePack, ok = parseVersion(s); ok {
			return typ, major, minor, servicePack, true
		}
	}
	return "", "", "", "", false
}

// parseVersion parses a version such as "FIX.4.4", "FIX.5.0SP2_EP254",
// "FIXT.1.1" or "FIX.Latest", whose major version is "Latest". An Extension
// Pack suffix is ignored.
func parseVersion(s string) (typ, major, minor, servicePack string, ok bool) {
	s, _, _ = strings.Cut(s, "_")
	typ, rest, ok := strings.Cut(s, ".")
	if !ok || typ != "FIX" && typ != "FIXT" {
		return "", "", "", "", false
	}
	if typ == "FIX" && rest == "Latest" {
		return typ, "Latest", "", "", true
	}
	major, rest, ok = strings.Cut(rest, ".")
	if !ok {
		return "", "", "", "", false
	}
	minor, servicePack, hasSP := strings.Cut(rest, "SP")
	if !isDigits(major) || !isDigits(minor) || hasSP && !isDigits(servicePack) {
		return "", "", "", "", false
	}
	return typ, major, minor, servicePack, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// enumDescription turns the name of an Orchestra code, in PascalCase, into
// a QuickFIX description, in upper snake case: words start at an upper case
// letter after a lower case letter or a digit, or before a lower case letter.
// PartiallyFilled becomes PARTIALLY_FILLED, ISINNumber ISIN_NUMBER and
// FIX50SP2 FIX50_SP2.
func enumDescription(name string) string {
	rs := []rune(name)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			next := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || unicode.IsUpper(prev) && next {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}
