package fix2

// Group represents a single instance within a FIX repeating group.
type Group struct {
	fields []Field
}

// Get returns the value of the first field with the given tag, or nil if not found.
func (g *Group) Get(tag int) []byte {
	for i := range g.fields {
		if g.fields[i].Tag == tag {
			return g.fields[i].Value
		}
	}
	return nil
}

// Fields returns the fields in this group instance.
func (g *Group) Fields() []Field {
	return g.fields
}
