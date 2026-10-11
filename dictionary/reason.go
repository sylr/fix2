package dictionary

// RejectReason classifies a validation error with the SessionRejectReason
// (tag 373) a session Reject carries for it. Not every FIX version defines
// every reason: a session sends only those its version knows.
type RejectReason int

const (
	ReasonInvalidTagNumber            RejectReason = 0
	ReasonRequiredTagMissing          RejectReason = 1
	ReasonTagNotDefinedForMessageType RejectReason = 2
	ReasonUndefinedTag                RejectReason = 3
	ReasonTagSpecifiedWithoutAValue   RejectReason = 4
	ReasonValueIsIncorrect            RejectReason = 5
	ReasonIncorrectDataFormat         RejectReason = 6
	ReasonDecryptionProblem           RejectReason = 7
	ReasonSignatureProblem            RejectReason = 8
	ReasonCompIDProblem               RejectReason = 9
	ReasonSendingTimeAccuracyProblem  RejectReason = 10
	ReasonInvalidMsgType              RejectReason = 11
	ReasonXMLValidationError          RejectReason = 12
	ReasonTagAppearsMoreThanOnce      RejectReason = 13
	ReasonTagSpecifiedOutOfOrder      RejectReason = 14
	ReasonRepeatingGroupOutOfOrder    RejectReason = 15
	ReasonIncorrectNumInGroupCount    RejectReason = 16
	ReasonNonDataValueIncludesSOH     RejectReason = 17
	ReasonInvalidApplVerID            RejectReason = 18
	ReasonOther                       RejectReason = 99
)

// reasonTexts are the reasons' names in the FIX specification, which a
// session Reject's Text starts with.
var reasonTexts = map[RejectReason]string{
	ReasonInvalidTagNumber:            "Invalid tag number",
	ReasonRequiredTagMissing:          "Required tag missing",
	ReasonTagNotDefinedForMessageType: "Tag not defined for this message type",
	ReasonUndefinedTag:                "Undefined tag",
	ReasonTagSpecifiedWithoutAValue:   "Tag specified without a value",
	ReasonValueIsIncorrect:            "Value is incorrect (out of range) for this tag",
	ReasonIncorrectDataFormat:         "Incorrect data format for value",
	ReasonDecryptionProblem:           "Decryption problem",
	ReasonSignatureProblem:            "Signature problem",
	ReasonCompIDProblem:               "CompID problem",
	ReasonSendingTimeAccuracyProblem:  "SendingTime accuracy problem",
	ReasonInvalidMsgType:              "Invalid MsgType",
	ReasonXMLValidationError:          "XML Validation error",
	ReasonTagAppearsMoreThanOnce:      "Tag appears more than once",
	ReasonTagSpecifiedOutOfOrder:      "Tag specified out of required order",
	ReasonRepeatingGroupOutOfOrder:    "Repeating group fields out of order",
	ReasonIncorrectNumInGroupCount:    "Incorrect NumInGroup count for repeating group",
	ReasonNonDataValueIncludesSOH:     "Non-data value includes field delimiter (SOH character)",
	ReasonInvalidApplVerID:            "Invalid or unsupported Application Version",
	ReasonOther:                       "Other",
}

// String returns the reason's name in the FIX specification, e.g. "Required
// tag missing".
func (r RejectReason) String() string {
	if s, ok := reasonTexts[r]; ok {
		return s
	}
	return "Other"
}
