package fix

import (
	"fmt"
	"testing"
)

// groupsWithMap is the former Groups implementation that builds a map[int]bool
// for child tag lookup, kept here to benchmark against slices.Contains.
func (m *Message) groupsWithMap(counterTag int, childTags []int) []Group {
	if len(childTags) == 0 {
		return nil
	}
	delimTag := childTags[0]

	childSet := make(map[int]bool, len(childTags))
	for _, t := range childTags {
		childSet[t] = true
	}

	counterIdx := -1
	count := 0
	for i := range m.fields {
		if m.fields[i].Tag == counterTag {
			counterIdx = i
			count, _ = ParseInt(m.fields[i].Value)
			break
		}
	}
	if counterIdx < 0 || count == 0 {
		return nil
	}

	var groups []Group
	var cur []Field
	for i := counterIdx + 1; i < len(m.fields) && len(groups) < count; i++ {
		f := m.fields[i]
		if !childSet[f.Tag] {
			break
		}
		if f.Tag == delimTag {
			if cur != nil {
				groups = append(groups, Group{fields: cur})
			}
			cur = []Field{f}
		} else if cur != nil {
			cur = append(cur, f)
		}
	}
	if cur != nil && len(groups) < count {
		groups = append(groups, Group{fields: cur})
	}
	return groups
}

// groupMsg builds a parsed message with N group instances, each having
// the given number of child fields per entry.
func groupMsg(b *testing.B, instances, fieldsPerEntry int) (*Message, []int) {
	b.Helper()
	bld := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001")

	// Child tags: 79 (delimiter), 80, 366, 467, 5001, 5002, ...
	baseTags := []int{79, 80, 366, 467}
	childTags := make([]int, fieldsPerEntry)
	for i := range fieldsPerEntry {
		if i < len(baseTags) {
			childTags[i] = baseTags[i]
		} else {
			childTags[i] = 5000 + i
		}
	}

	bld.SetInt(78, instances)
	for range instances {
		for _, tag := range childTags {
			bld.AppendString(tag, "value")
		}
	}

	raw := bld.Build()
	msg, err := Parse(raw)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	return &msg, childTags
}

func BenchmarkGroups(b *testing.B) {
	for _, tc := range []struct {
		instances int
		fields    int
	}{
		{2, 4},
		{10, 4},
		{50, 4},
		{2, 16},
		{10, 16},
	} {
		msg, childTags := groupMsg(b, tc.instances, tc.fields)
		name := fmt.Sprintf("instances=%d/fields=%d", tc.instances, tc.fields)

		b.Run("map/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				msg.groupsWithMap(78, childTags)
			}
		})
		b.Run("slices/"+name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				msg.Groups(78, childTags)
			}
		})
	}
}

func BenchmarkGroupsOf(b *testing.B) {
	for _, tc := range []struct {
		instances int
		fields    int
	}{
		{1, 4},
		{2, 4},
		{10, 4},
		{50, 4},
		{200, 4},
		{10, 16},
	} {
		msg, childTags := groupMsg(b, tc.instances, tc.fields)
		spec := &GroupSpec{Counter: 78, Delim: childTags[0], Tags: childTags}
		b.Run(fmt.Sprintf("instances=%d/fields=%d", tc.instances, tc.fields), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				msg.GroupsOf(spec)
			}
		})
	}
}
