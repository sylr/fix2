// Package dicttest compares dictionaries in tests.
package dicttest

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/sylr/fix2/dictionary"
)

// Diff describes the first difference between the exported contents of a and
// b, or returns "" if they have the same. Compiled tables are not compared:
// they are built from the exported contents.
func Diff(a, b *dictionary.Dict) string {
	for _, c := range []struct {
		name string
		a, b any
	}{
		{"Type", a.Type, b.Type},
		{"Major", a.Major, b.Major},
		{"Minor", a.Minor, b.Minor},
		{"ServicePack", a.ServicePack, b.ServicePack},
		{"Fields", a.Fields, b.Fields},
		{"Header", a.Header, b.Header},
		{"Trailer", a.Trailer, b.Trailer},
		{"HeaderGroups", a.HeaderGroups, b.HeaderGroups},
		{"TrailerGroups", a.TrailerGroups, b.TrailerGroups},
		{"HeaderConditions", a.HeaderConditions, b.HeaderConditions},
		{"TrailerConditions", a.TrailerConditions, b.TrailerConditions},
		{"message types", slices.Sorted(maps.Keys(a.Messages)), slices.Sorted(maps.Keys(b.Messages))},
	} {
		if !reflect.DeepEqual(c.a, c.b) {
			return fmt.Sprintf("%s differ:\n%#v\n%#v", c.name, c.a, c.b)
		}
	}
	for mt, am := range a.Messages {
		bm := b.Messages[mt]
		for _, c := range []struct {
			name string
			a, b any
		}{
			{"Name", am.Name, bm.Name},
			{"MsgType", am.MsgType, bm.MsgType},
			{"MsgCat", am.MsgCat, bm.MsgCat},
			{"Fields", am.Fields, bm.Fields},
			{"Groups", am.Groups, bm.Groups},
			{"Conditions", am.Conditions, bm.Conditions},
		} {
			if !reflect.DeepEqual(c.a, c.b) {
				return fmt.Sprintf("message %s: %s differ:\n%#v\n%#v", mt, c.name, c.a, c.b)
			}
		}
	}
	return ""
}
