package dictionary

import "maps"

// Compose returns the dictionary validating the application messages of app
// as FIXT carries them: transport's standard header and trailer, with their
// groups and conditions, around app's messages. FIX 5.0 application
// dictionaries leave their header and trailer empty, for the transport
// dictionary to provide.
//
// transport's own messages (Logon, Heartbeat...) are kept and win over app
// messages of the same MsgType. Fields come from both; for a tag both define,
// app's definition is used, with the enums of both (no restriction if either
// has none, nil or empty). The result takes app's version. transport and app are not
// changed.
func Compose(transport, app *Dict) *Dict {
	d := &Dict{
		Type:              app.Type,
		Major:             app.Major,
		Minor:             app.Minor,
		ServicePack:       app.ServicePack,
		Fields:            make(map[int]*FieldDef, len(transport.Fields)+len(app.Fields)),
		Messages:          make(map[string]*MessageDef, len(transport.Messages)+len(app.Messages)),
		Header:            transport.Header,
		Trailer:           transport.Trailer,
		HeaderGroups:      transport.HeaderGroups,
		TrailerGroups:     transport.TrailerGroups,
		HeaderConditions:  transport.HeaderConditions,
		TrailerConditions: transport.TrailerConditions,
	}
	maps.Copy(d.Fields, transport.Fields)
	for tag, fd := range app.Fields {
		t := d.Fields[tag]
		if t == nil {
			d.Fields[tag] = fd
			continue
		}
		merged := *fd
		if len(fd.Enums) > 0 && len(t.Enums) > 0 {
			merged.Enums = maps.Clone(t.Enums)
			maps.Copy(merged.Enums, fd.Enums)
		} else {
			merged.Enums = nil
		}
		d.Fields[tag] = &merged
	}
	// New compiles tables into each MessageDef: copy them so that neither
	// transport's nor app's are touched.
	for _, src := range []*Dict{app, transport} {
		for mt, md := range src.Messages {
			d.Messages[mt] = &MessageDef{
				Name:       md.Name,
				MsgType:    md.MsgType,
				MsgCat:     md.MsgCat,
				Fields:     md.Fields,
				Groups:     md.Groups,
				Conditions: md.Conditions,
			}
		}
	}
	return New(d)
}
