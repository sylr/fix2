package fix2

// dataFields lists the standard FIX DATA fields with the LENGTH field that
// precedes each one and gives its size in bytes. A DATA value may contain SOH,
// so the parser must read exactly that many bytes instead of scanning for the
// next SOH.
//
// The pairs come from the FIX Trading Community's FIX Latest Orchestra
// repository (OrchestraFIXLatest.xml, each data field's lengthId), plus
// EncodedMDEntryStatusText whose lengthId is missing there and
// EncodedSymbolLen/EncodedSymbol from the QuickFIX FIX 5.0 SP2 dictionary.
//
// Left out on purpose because QuickFIX's FIX 5.0 SP2 dictionary gives those tag
// numbers a different meaning than the FIX repository, so either reading
// would misparse the other's messages: 1525/1527 (EncodedDocumentationText vs
// ContextPartyRole/ContextPartySubID), 1578/1579 (EncodedEventText vs
// RelatedContextPartyRole/NoRelatedContextPartySubIDs) and 1618/1619
// (NoThrottleMsgType/ThrottleMsgType vs RelationshipRiskEncodedSecurityDesc).
var dataFields = []struct{ length, data int }{
	{90, 91},       // SecureDataLen, SecureData
	{93, 89},       // SignatureLength, Signature
	{95, 96},       // RawDataLength, RawData
	{212, 213},     // XmlDataLen, XmlData
	{348, 349},     // EncodedIssuerLen, EncodedIssuer
	{350, 351},     // EncodedSecurityDescLen, EncodedSecurityDesc
	{352, 353},     // EncodedListExecInstLen, EncodedListExecInst
	{354, 355},     // EncodedTextLen, EncodedText
	{356, 357},     // EncodedSubjectLen, EncodedSubject
	{358, 359},     // EncodedHeadlineLen, EncodedHeadline
	{360, 361},     // EncodedAllocTextLen, EncodedAllocText
	{362, 363},     // EncodedUnderlyingIssuerLen, EncodedUnderlyingIssuer
	{364, 365},     // EncodedUnderlyingSecurityDescLen, EncodedUnderlyingSecurityDesc
	{445, 446},     // EncodedListStatusTextLen, EncodedListStatusText
	{618, 619},     // EncodedLegIssuerLen, EncodedLegIssuer
	{621, 622},     // EncodedLegSecurityDescLen, EncodedLegSecurityDesc
	{1184, 1185},   // SecurityXMLLen, SecurityXML
	{1277, 1278},   // DerivativeEncodedIssuerLen, DerivativeEncodedIssuer
	{1280, 1281},   // DerivativeEncodedSecurityDescLen, DerivativeEncodedSecurityDesc
	{1282, 1283},   // DerivativeSecurityXMLLen, DerivativeSecurityXML
	{1359, 1360},   // EncodedSymbolLen, EncodedSymbol
	{1397, 1398},   // EncodedMktSegmDescLen, EncodedMktSegmDesc
	{1401, 1402},   // EncryptedPasswordLen, EncryptedPassword
	{1403, 1404},   // EncryptedNewPasswordLen, EncryptedNewPassword
	{1468, 1469},   // EncodedSecurityListDescLen, EncodedSecurityListDesc
	{1620, 1621},   // InstrumentScopeEncodedSecurityDescLen, InstrumentScopeEncodedSecurityDesc
	{1664, 1665},   // EncodedRejectTextLen, EncodedRejectText
	{1678, 1697},   // EncodedOptionExpirationDescLen, EncodedOptionExpirationDesc
	{1733, 1734},   // EncodedFirmAllocTextLen, EncodedFirmAllocText
	{1871, 1872},   // LegSecurityXMLLen, LegSecurityXML
	{1874, 1875},   // UnderlyingSecurityXMLLen, UnderlyingSecurityXML
	{2072, 2073},   // EncodedUnderlyingEventTextLen, EncodedUnderlyingEventText
	{2074, 2075},   // EncodedLegEventTextLen, EncodedLegEventText
	{2111, 2112},   // EncodedAttachmentLen, EncodedAttachment
	{2179, 2180},   // EncodedLegOptionExpirationDescLen, EncodedLegOptionExpirationDesc
	{2287, 2288},   // EncodedUnderlyingOptionExpirationDescLen, EncodedUnderlyingOptionExpirationDesc
	{2351, 2352},   // EncodedComplianceTextLen, EncodedComplianceText
	{2372, 2371},   // EncodedTradeContinuationTextLen, EncodedTradeContinuationText
	{2481, 2482},   // EncodedMDStatisticDescLen, EncodedMDStatisticDesc
	{2494, 2493},   // EncodedLegDocumentationTextLen, EncodedLegDocumentationText
	{2522, 2521},   // EncodedWarningTextLen, EncodedWarningText
	{2637, 2638},   // EncodedMiscFeeSubTypeDescLen, EncodedMiscFeeSubTypeDesc
	{2651, 2652},   // EncodedCommissionDescLen, EncodedCommissionDesc
	{2665, 2666},   // EncodedAllocCommissionDescLen, EncodedAllocCommissionDesc
	{2715, 2716},   // EncodedFinancialInstrumentFullNameLen, EncodedFinancialInstrumentFullName
	{2718, 2719},   // EncodedLegFinancialInstrumentFullNameLen, EncodedLegFinancialInstrumentFullName
	{2721, 2722},   // EncodedUnderlyingFinancialInstrumentFullNameLen, EncodedUnderlyingFinancialInstrumentFullName
	{2797, 2798},   // EncodedMatchExceptionTextLen, EncodedMatchExceptionText
	{2802, 2801},   // EncodedReplaceTextLen, EncodedReplaceText
	{2809, 2808},   // EncodedCancelTextLen, EncodedCancelText
	{2815, 2814},   // EncodedPostTradePaymentDescLen, EncodedPostTradePaymentDesc
	{2971, 2972},   // EncodedSettlStatusReasonTextLen, EncodedSettlStatusReasonText
	{3108, 3109},   // EncodedMDEntryStatusTextLen, EncodedMDEntryStatusText
	{40004, 40005}, // EncodedAdditionalTermBondDescLen, EncodedAdditionalTermBondDesc
	{40008, 40009}, // EncodedAdditionalTermBondIssuerLen, EncodedAdditionalTermBondIssuer
	{40978, 40979}, // EncodedLegStreamTextLen, EncodedLegStreamText
	{40980, 40981}, // EncodedLegProvisionTextLen, EncodedLegProvisionText
	{40982, 40983}, // EncodedStreamTextLen, EncodedStreamText
	{40984, 40985}, // EncodedPaymentTextLen, EncodedPaymentText
	{40986, 40987}, // EncodedProvisionTextLen, EncodedProvisionText
	{40988, 40989}, // EncodedUnderlyingStreamTextLen, EncodedUnderlyingStreamText
	{41083, 41084}, // EncodedDeliveryStreamCycleDescLen, EncodedDeliveryStreamCycleDesc
	{41101, 41102}, // EncodedMarketDisruptionFallbackUnderlierSecurityDescLen, EncodedMarketDisruptionFallbackUnderlierSecurityDesc
	{41107, 41108}, // EncodedExerciseDescLen, EncodedExerciseDesc
	{41256, 41257}, // EncodedStreamCommodityDescLen, EncodedStreamCommodityDesc
	{41320, 41321}, // EncodedLegAdditionalTermBondDescLen, EncodedLegAdditionalTermBondDesc
	{41324, 41325}, // EncodedLegAdditionalTermBondIssuerLen, EncodedLegAdditionalTermBondIssuer
	{41458, 41459}, // EncodedLegDeliveryStreamCycleDescLen, EncodedLegDeliveryStreamCycleDesc
	{41476, 41477}, // EncodedLegMarketDisruptionFallbackUnderlierSecurityDescLen, EncodedLegMarketDisruptionFallbackUnderlierSecurityDesc
	{41482, 41483}, // EncodedLegExerciseDescLen, EncodedLegExerciseDesc
	{41653, 41654}, // EncodedLegStreamCommodityDescLen, EncodedLegStreamCommodityDesc
	{41710, 41711}, // EncodedUnderlyingAdditionalTermBondDescLen, EncodedUnderlyingAdditionalTermBondDesc
	{41806, 41807}, // EncodedUnderlyingDeliveryStreamCycleDescLen, EncodedUnderlyingDeliveryStreamCycleDesc
	{41811, 41812}, // EncodedUnderlyingExerciseDescLen, EncodedUnderlyingExerciseDesc
	{41873, 41874}, // EncodedUnderlyingMarketDisruptionFallbackUnderlierSecDescLen, EncodedUnderlyingMarketDisruptionFallbackUnderlierSecurityDesc
	{41969, 41970}, // EncodedUnderlyingStreamCommodityDescLen, EncodedUnderlyingStreamCommodityDesc
	{42025, 42026}, // EncodedUnderlyingAdditionalTermBondIssuerLen, EncodedUnderlyingAdditionalTermBondIssuer
	{42171, 42172}, // EncodedUnderlyingProvisionTextLen, EncodedUnderlyingProvisionText
	{42451, 42452}, // LegPaymentStreamFormulaImageLength, LegPaymentStreamFormulaImage
	{42652, 42653}, // PaymentStreamFormulaImageLength, PaymentStreamFormulaImage
	{42947, 42948}, // UnderlyingPaymentStreamFormulaImageLength, UnderlyingPaymentStreamFormulaImage
	{43109, 42684}, // PaymentStreamFormulaLength, PaymentStreamFormula
	{43110, 42486}, // LegPaymentStreamFormulaLength, LegPaymentStreamFormula
	{43111, 42982}, // UnderlyingPaymentStreamFormulaLength, UnderlyingPaymentStreamFormula
}

// maxDataTag is the highest tag in dataFields.
const maxDataTag = 43111

// isDataTag marks DATA tags in a Profile's lookup table.
const isDataTag = -1

// DataLengthTag returns the LENGTH tag that announces DATA tag dataTag in the
// default profile, or 0 if dataTag is not a DATA tag the parser reads by
// length. Builders use it to emit the pair the way the parser expects.
func DataLengthTag(dataTag int) int { return defaultProfile.DataLengthTag(dataTag) }

// DataTag returns the DATA tag announced by LENGTH tag lengthTag in the
// default profile, or 0 if lengthTag is not such a LENGTH tag.
func DataTag(lengthTag int) int { return defaultProfile.DataTag(lengthTag) }
