package fix2

// dataFields lists the standard FIX DATA fields with the LENGTH field that
// precedes each one and gives its size in bytes. A DATA value may contain SOH,
// so the parser must read exactly that many bytes instead of scanning for the
// next SOH. The pairs are fixed by the FIX specification (FIX 4.0 to 5.0 SP2);
// all but EncodedSymbol were extracted from the QuickFIX dictionaries.
var dataFields = []struct{ length, data int }{
	{90, 91},     // SecureDataLen, SecureData
	{93, 89},     // SignatureLength, Signature
	{95, 96},     // RawDataLength, RawData
	{212, 213},   // XmlDataLen, XmlData
	{348, 349},   // EncodedIssuerLen, EncodedIssuer
	{350, 351},   // EncodedSecurityDescLen, EncodedSecurityDesc
	{352, 353},   // EncodedListExecInstLen, EncodedListExecInst
	{354, 355},   // EncodedTextLen, EncodedText
	{356, 357},   // EncodedSubjectLen, EncodedSubject
	{358, 359},   // EncodedHeadlineLen, EncodedHeadline
	{360, 361},   // EncodedAllocTextLen, EncodedAllocText
	{362, 363},   // EncodedUnderlyingIssuerLen, EncodedUnderlyingIssuer
	{364, 365},   // EncodedUnderlyingSecurityDescLen, EncodedUnderlyingSecurityDesc
	{445, 446},   // EncodedListStatusTextLen, EncodedListStatusText
	{618, 619},   // EncodedLegIssuerLen, EncodedLegIssuer
	{621, 622},   // EncodedLegSecurityDescLen, EncodedLegSecurityDesc
	{1184, 1185}, // SecurityXMLLen, SecurityXML
	{1277, 1278}, // DerivativeEncodedIssuerLen, DerivativeEncodedIssuer
	{1280, 1281}, // DerivativeEncodedSecurityDescLen, DerivativeEncodedSecurityDesc
	{1282, 1283}, // DerivativeSecurityXMLLen, DerivativeSecurityXML
	{1359, 1360}, // EncodedSymbolLen, EncodedSymbol
	{1397, 1398}, // EncodedMktSegmDescLen, EncodedMktSegmDesc
	{1401, 1402}, // EncryptedPasswordLen, EncryptedPassword
	{1403, 1404}, // EncryptedNewPasswordLen, EncryptedNewPassword
	{1468, 1469}, // EncodedSecurityListDescLen, EncodedSecurityListDesc
	{1618, 1619}, // RelationshipRiskEncodedSecurityDescLen, RelationshipRiskEncodedSecurityDesc
	{1620, 1621}, // RiskEncodedSecurityDescLen, RiskEncodedSecurityDesc
}

// isDataTag marks DATA tags in dataTagFor.
const isDataTag = -1

// dataTagFor classifies tags for the parser with a single array lookup: a
// LENGTH tag maps to the DATA tag it announces, a DATA tag to isDataTag, any
// other tag to 0.
var dataTagFor = func() (t [1622]int16) {
	for _, p := range dataFields {
		t[p.length] = int16(p.data)
		t[p.data] = isDataTag
	}
	return t
}()
