package main

// goTypeInfo describes how a FIX type maps to Go.
type goTypeInfo struct {
	GoType   string // e.g. "string", "int", "float64", "bool", "time.Time", "[]byte"
	Category string // string, int, float, bool, time, bytes
}

// fixTypeMap maps FIX type names to Go type info.
var fixTypeMap = map[string]goTypeInfo{
	// string types
	"STRING":              {GoType: "string", Category: "string"},
	"CHAR":                {GoType: "string", Category: "string"},
	"CURRENCY":            {GoType: "string", Category: "string"},
	"EXCHANGE":            {GoType: "string", Category: "string"},
	"COUNTRY":             {GoType: "string", Category: "string"},
	"MULTIPLEVALUESTRING": {GoType: "string", Category: "string"},
	"MONTHYEAR":           {GoType: "string", Category: "string"},

	// int types
	"INT":        {GoType: "int", Category: "int"},
	"LENGTH":     {GoType: "int", Category: "int"},
	"SEQNUM":     {GoType: "int", Category: "int"},
	"NUMINGROUP": {GoType: "int", Category: "int"},
	"DAYOFMONTH": {GoType: "int", Category: "int"},

	// float types
	"FLOAT":      {GoType: "float64", Category: "float"},
	"PRICE":      {GoType: "float64", Category: "float"},
	"QTY":        {GoType: "float64", Category: "float"},
	"AMT":        {GoType: "float64", Category: "float"},
	"PERCENTAGE": {GoType: "float64", Category: "float"},

	// bool
	"BOOLEAN": {GoType: "bool", Category: "bool"},

	// time types
	"UTCTIMESTAMP": {GoType: "time.Time", Category: "time"},
	"UTCDATE":      {GoType: "time.Time", Category: "time"},
	"UTCTIMEONLY":  {GoType: "time.Time", Category: "time"},
	"LOCALMKTDATE": {GoType: "time.Time", Category: "time"},

	// FIX 4.0/4.1 names: TIME became UTCTIMESTAMP in FIX 4.2, DATE was
	// replaced by LOCALMKTDATE/UTCDATE.
	"TIME": {GoType: "time.Time", Category: "time"},
	"DATE": {GoType: "time.Time", Category: "time"},

	// bytes
	"DATA": {GoType: "[]byte", Category: "bytes"},
}

// lookupType returns the Go type info for a FIX type, defaulting to string.
func lookupType(fixType string) goTypeInfo {
	if info, ok := fixTypeMap[fixType]; ok {
		return info
	}
	return goTypeInfo{GoType: "string", Category: "string"}
}
