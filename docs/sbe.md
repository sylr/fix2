# fix2gen sbe

`fix2gen sbe` generates Go codecs for the messages of a Simple Binary
Encoding schema: [SBE 1.0](https://www.fixtrading.org/wp-content/uploads/download-manager-files/Simple_Binary_Encoding_V1.0_with_Errata_November_2020.pdf)
(FIX Technical Standard, with the November 2020 errata). Section numbers
below (§) are its own. FIXP's session messages are its first user
([docs/fixp.md](fixp.md)), venues' application schemas the next.

    fix2gen sbe -schema <xml-path> -pkg <package-name> [-out <dir>]
        [-tolerate-foreign-metadata] [-tolerate-undeclared-primitive-types]
        [-tolerate-group-redefinitions] [-tolerate-field-redefinitions]
        [-tolerate-unfilled-counts] [-assume-same-encodings]

By default it follows SBE 1.0 strictly. Six flags, off by default, each
change what it accepts or what the decoders it generates do, and are
named in the generated package's doc comment:

| Flag | Does | Kind |
|---|---|---|
| `-tolerate-foreign-metadata` | loads a schema holding elements and attributes of other namespaces, and ignores them (see [Schemas](#schemas)) | a toleration: sbe.xsd, normative, allows none |
| `-tolerate-undeclared-primitive-types` | resolves a field's `type` naming a primitive, such as `uint32`, that no encoding is named, as that primitive (see [Schemas](#schemas)) | a toleration: a field's type names an encoding of `<types>` (§6.7, sbe.xsd) |
| `-tolerate-group-redefinitions` | loads a schema where a group's name or id stands for different groups, and generates a type for each (see [Generated code](#generated-code)) | a toleration: a group's name and id identify one group throughout a schema (§6.8, §6.9) |
| `-tolerate-field-redefinitions` | loads a schema where a field's name or id stands for fields of different ids, names or encodings, `<data>` included (see [Schemas](#schemas)) | a toleration: a field's name and id identify one field, of one encoding, throughout a schema (§6.7, §6.9) |
| `-assume-same-encodings` | decoders skip a newer version's groups and data, which the header's and dimensions' counts tell of, by the schema's `groupSizeEncoding` and data length (see [Decoding](#decoding)) | an assumption SBE does not guarantee: a newer version encoding them otherwise is misread |
| `-tolerate-unfilled-counts` | decoders take a header's or dimension's `numGroups` or `numVarDataFields` of 0, where the received version holds groups or data, for a count left unfilled, as Real Logic's encoders leave them, rather than `ErrMalformed` (see [Decoding](#decoding)) | a toleration: such counts contradict the message |

## Generated code

Each message is a Go struct, encoded and decoded whole. A struct decoded
into again reuses what it holds: once warm, decoding allocates nothing.
Flyweights over the buffer, as Real Logic's generator also offers, may
come later, if benchmarks call for them.

| Schema | Go |
|---|---|
| `<type>` scalar | its primitive: `int8`…`uint64`, `float32`, `float64`, `byte` for `char` |
| `<type length="n">`, n > 1 | `[n]T`; for a `char` array without `characterEncoding`, or with US-ASCII, an `XString()` method trimming at the first NUL |
| `<enum>` | a named type over its encoding (`char` or unsigned), a constant per valid value, `String()`, `Known()` |
| `<set>` | a named type over its unsigned encoding, a constant per choice (its bit) |
| `<composite>` | a struct of its elements, `<ref>` included |
| optional scalar or enum | `Optional[T]{Value T; Valid bool}`, the zero value encoding null |
| optional composite | the composite, whose first element is an optional scalar: the composite is null when that element is (§6.5.4.4) |
| optional set | the set: SBE has no null set but the empty one (§4.14.2) |
| constant | no field: a method returning the constant |
| `<group>` | a slice of entry structs, one type per group name: a name and an id define one group throughout a schema (§6.8, §6.9), which loading checks. With `-tolerate-group-redefinitions`, each group of a name standing for different groups has a type of its own, named by its place (below) |
| `<data>` | `[]byte`, decoded as a subslice of the input |
| message | a struct, its template ID as a constant, `AppendTo(dst) ([]byte, error)`, `Decode(b) (int, error)`, `DecodeBody(h, body) (int, error)` |
| `messageHeader` | `MessageHeader`, `ReadHeader(b) (MessageHeader, []byte, error)`. A header with elements beside SBE's six (§5.2), such as Binance's `seqNum` and `sendingTime`, gives each message `AppendWithHeader(dst, h) ([]byte, error)`: SBE's elements are set from the message and the schema, the others written from `h`; `AppendTo` writes them zero |

Names are the schema's, exported: `ClOrdID`, `SideEnum`, `SideEnumBuy`.
A group's type is its name, exported, but where `-tolerate-group-redefinitions`
let one name stand for different groups: each group of that name, nested
ones included, is then named by its place, the names of its message and
of the groups enclosing it before its own, exported and joined. Binance's
Orders are `NewOrderListOrders` and `ListStatusOrders`, its MDEntriesBids
`MarketDataSnapshotMDEntriesBids`, `MarketDataIncrementalBookTickerMDEntriesBids`
and `MarketDataIncrementalDepthMDEntriesBids`. Every group of such a name
has its own type, even one identical to another: the type's name tells
where the group is. Groups of a name all the same share one type, as
without the flag (Binance's ListTriggeringInstructions, in both Orders);
a group whose type is shared is generated from its first place, in schema
order, and so are the names of the groups it holds. The members keep the
schema's names: `NewOrderList.Orders` is a `[]NewOrderListOrders`.
Two schema names giving one Go name, a member named as a generated
method (`AppendTo`, `AppendWithHeader` where generated, `Decode`,
`DecodeBody`, `TemplateID`, a constant's or `XString`'s method), two
groups' places joining into one name (message `AB`'s group `C`, message
`A`'s group `BC`), or two messages giving one file name (`FooBar` and
`Foo_Bar`, both `msg_foo_bar.go`), is a generation error.

Null is the schema's `nullValue`, or the default of §4.4.2, an optional
field of a required type included: the lowest value of a signed integer,
the highest of an unsigned one, NUL for a `char`, NaN for a float (any NaN
decodes as null). An optional composite is null when its first element
is (§6.5.4.4); its other elements are encoded and decoded as they are, as
Real Logic's codecs do, null or not.

## Decoding

`Decode(b)` reads the header, checks its schema id and template id, and
decodes the body. `DecodeBody(h, body)` decodes a body whose header the
caller read, checking the template id. Both return the length they read:
`Decode` the message's, header included, `DecodeBody` the body's. It is
the root block of the header's `blockLength`, padding or a newer
version's fields included, then the groups and data, entries counted by
their dimension's `blockLength` likewise, and, with
`-assume-same-encodings`, the groups and data of a newer version the
decoder skips (below). On error, the struct is left
zero and the length is 0. A decoded struct's `[]byte` fields alias the
input: they are valid until the input is changed or reused.

Until S3 of the strict-by-default effort
([docs/conformance.md](conformance.md#milestones)), `Decode` and
`DecodeBody` returned the error alone: a caller not needing the length
discards it, `_, err := m.Decode(b)`.

Every read is bounded by the input. A message, group entry or field that
does not fit is an error, never a panic or a read past the input.

- The root block is the header's `blockLength` long, not the schema's: a
  newer encoder may append fields, an older one omit them (§7.3.1).
- A field, group or data field whose `sinceVersion` is above the header's
  `version` is absent: zero, or not `Valid` if optional, and an absent
  group or data field takes no bytes (§7.4). A field the version promises
  that the block does not hold is malformed: an error. Decoding a struct
  again clears what the new message does not hold.
- A group entry is its dimension's `blockLength` long, likewise. An entry
  count above the dimension's `maxValue` or below its `minValue` is an
  error; without `maxValue`, any count the type holds is accepted (see
  [Encoding](#encoding)). A data length beyond its length's `minValue`
  and `maxValue` is too.
- Header and dimension fields may be of any unsigned width: a block
  length or a count is checked against the bytes left before it becomes
  an `int`, which a 64-bit one, or a 32-bit one on a 32-bit platform,
  would wrap. A version beyond 2³¹−1 reads as 2³¹−1.
- Allocation is bounded twice: an entry takes at least one byte on the
  wire, or its group must be empty, so a count cannot exceed the bytes
  left; and a message decodes at most `MaxEntries` entries across its
  groups, a constant of the generated package (65535 by default), so an
  older version's one-byte entries cannot fill large structs without
  limit.
- Where the header or a group's dimension has `numGroups` or
  `numVarDataFields`, it is the number of groups or data fields the
  received version holds there, those whose `sinceVersion` is not above
  it (§7.3.2), in a message of any version: another count is
  `ErrMalformed`, but for a newer version's groups and data (below).
  Real Logic's encoders leave these counts zero unless the application
  sets them: `-tolerate-unfilled-counts` takes a count of 0 for one the
  encoder did not fill, and the decoder reads what it knows the version
  holds.
- A newer version, a header `version` above the schema's, may add fixed
  fields at the end of a block: the block's `blockLength` skips them
  (§7.3.1). It may add groups after the groups and data after the data,
  of the message or of a group's entries, each of a type of its own:
  new fields may have new types (§7.1.1), a group another `dimensionType`
  (§5.4.10.2). A decoder finds them only by the `numGroups` and
  `numVarDataFields` counts a header or dimension may carry (§7.3.2,
  "This supports a walk by a decoder of all the elements of a message"),
  and cannot bound them without knowing their types. So, in a message of
  a newer version, a decoder returns `ErrNewer`:
  - where the header or the dimension counts them, for a count above the
    groups or data the schema knows: the newer version's cannot be
    bounded (below, the count is malformed, as in any version);
  - where nothing counts them, wherever they could come before something
    the decoder reads: groups before a message's data, groups and data in
    a group's entries, which the next entry, or what follows the group,
    follows. A message's data, or its groups when it has no data, are
    followed by nothing: a newer version's groups or data after them
    cannot be told from bytes after the message, and are not read, nor
    counted in the length, the caller deciding.

  `-assume-same-encodings` assumes instead that a newer version encodes
  its groups and data as the schema does: where the header or dimension
  counts them, a count above the schema's tells of groups and data the
  decoder skips, and counts in its length, a group by SBE's default
  dimension, `groupSizeEncoding`, its entries by its `blockLength`,
  `numGroups` and `numVarDataFields`, recursively, bounded as the
  schema's entries are, `MaxEntries` included; data by the length type
  all the schema's data encodings share. A schema without a
  `groupSizeEncoding` that has both counts, or whose data lengths are of
  several types, cannot skip them: `ErrNewer`. A newer version that does
  encode them otherwise is misread, silently or not: a `uint32` length
  `02 00 00 00 58 59` read as a `uint16` skips 4 bytes, not 6. Without
  counts, as by default.

  A count of 0 that `-tolerate-unfilled-counts` takes for unfilled tells
  nothing of a newer version's groups and data: alone, the flag leaves
  them as where nothing counts them, `ErrNewer` where they could come
  before what the decoder reads; with `-assume-same-encodings`, they are
  taken to be none, and a newer version's group before the message's
  data is misread, its dimension read as the data's length.

  A schema meant to evolve with groups and data puts the counts in its
  header and dimensions. FIXP's has none, and is at version 0: a FIXP
  message of a newer version holding data, as Negotiate, is `ErrNewer`.
- Values are not validated: an unknown enum value or a null in a required
  field decodes as it is, `Known()` and the application tell (§4.15,
  §7.4). Structure is (§5.6).
- Bytes after the message are not read: the length returned tells where
  the message ends, and the caller decides what the rest is (see
  [FIXP](fixp.md#limits)).

## Encoding

`AppendTo` writes the header, with the schema's id and version and the
message's `blockLength`, then the body: fields at their offsets, padding
zeroed (§5.3.3), each group's dimension then its entries, each data
field's length then its bytes. A data field longer than its length type
allows, null excluded (254 for a `uint8`, 65534 for a `uint16`, or the
length's `maxValue`, §4.7.4, §4.8.3), or shorter than the length's
`minValue` (`ErrLength`), or a group whose entries fall outside its
dimension's bounds (`ErrEntries`), is an error; nothing is appended then. The
header's and dimensions' `numGroups` and `numVarDataFields`, when the
schema has them, are filled in.

A group holds at most 254 entries for a `uint8` count, 65534 for a
`uint16`, unless `numInGroup` has a `maxValue`. SBE says both that a count
goes up to its type's largest value (§5.4.10.1) and that a required
integer does not hold that value, its null (§4.4.2); Real Logic's codecs
refuse to encode it. fix2gen's encode at most the lower bound and decode
up to the higher one.

## Schemas

| Supported | Rejected, with an error naming the element |
|---|---|
| `<type>`: the 11 primitives, arrays, `length="0"` in data | a primitive SBE does not define |
| a field's `type` naming an encoding of `<types>` | a field's `type` naming a primitive no encoding is named (`uint32`), unless `-tolerate-undeclared-primitive-types` |
| `presence` required, optional (with `nullValue`) and constant (a value or `valueRef`) | `nullValue` on a required or constant type; a constant without a value |
| `<composite>` with `<type>`, `<enum>`, `<set>`, `<composite>` and `<ref>` | a composite without elements; a reference cycle |
| `<enum>` over `char` or an unsigned type; `<set>` over an unsigned type | other encodings |
| `offset`, `blockLength` | an offset overlapping the previous member; a `blockLength` under its fields' |
| `sinceVersion`, `deprecated` | either above the schema's version |
| `headerType`, `dimensionType` of the schema's | a header without unsigned `blockLength`, `templateId`, `schemaId`, `version`; a dimension without `blockLength` and `numInGroup` |
| both byte orders | `xi:include` |
| `minValue`, `maxValue`, `nullValue` | one its type cannot represent; a data length's or a count's `minValue` above its `maxValue` |
| | a group or data field before a field, a group after data (§5.5); duplicate names or ids; a group name or id defining two different groups, its members' versions included, unless `-tolerate-group-redefinitions`; a field name or id standing for another id, name or encoding, unless `-tolerate-field-redefinitions`; a header's `numGroups` or `numVarDataFields` that is not a required unsigned scalar (§5.2.1); ids, versions or block lengths the header or dimension cannot hold |

Elements and attributes are checked by their expanded names, against
sbe.xsd (§6.1). The root is `messageSchema` of SBE 1.0's namespace,
`http://fixprotocol.io/2016/sbe` (§6.2): another, such as SBE 2.0's
`http://fixprotocol.io/2017/sbe` or a release candidate's, or none, is an
error. `message` is of that namespace too, the other elements and every
attribute of none, as sbe.xsd declares them: an element of no namespace
where sbe.xsd has none, an attribute sbe.xsd does not give its element
(a misspelt `presense`), an SBE element or attribute qualified otherwise,
or a prefix never declared, is an error naming it. Elements and
attributes of another namespace, such as a venue's metadata, are errors
too: sbe.xsd declares no wildcard for them. `-tolerate-foreign-metadata`
(`LoadOptions.TolerateForeignMetadata`) accepts and ignores them, an
element with all it holds. Of XML Schema instance's attributes, which
XML Schema allows on any element, the location hints
`xsi:schemaLocation` and `xsi:noNamespaceSchemaLocation` are accepted and
ignored, tolerated or not; `xsi:nil` and `xsi:type`, which would change
validation, are refused, sbe.xsd declaring no element nillable and the
loader resolving no type they name, as is any other `xsi` name, such as
`xsi:presense`. `epoch` and `timeUnit` are accepted and ignored.

A field's `type` names an encoding of `<types>`: §6.7, "Must match the
name attribute of a simple <type>, <composite> encoding type, <enum> or
<set>"; sbe.xsd, "Must match the name of an encoding contained by
'types' element"; §6.9 lists as an error "A field or <enum> references a
type name that is undefined". SBE allows a primitive where it says so, an
enum's `encodingType` (§4.13.3, "In-line style: the value of
encodingType is its primitive datatype"), and gives the encodings by
which its examples' `type="uint32"` are valid (§4.4.4, "These are valid
encoding specifications for each of the integer primitive types", `<type
name="uint32" primitiveType="uint32"/>`). Binance's schema types fields
`uint32`, `int32`, `int64`, `uint64`, `uint16` and `uint8` without
declaring them, as Real Logic's SbeTool, which predefines the eleven
primitives, accepts. `-tolerate-undeclared-primitive-types`
(`LoadOptions.TolerateUndeclaredPrimitiveTypes`) resolves such a name as
that encoding would be: a required scalar of the primitive, the field's
`presence` applying, an optional one null as its primitive is (§4.4.2),
one type per primitive, so that groups using it compare equal. An
encoding the schema names so is the field's type, tolerated or not. A
`<data>`'s or `<ref>`'s type still names an encoding.

A group's name and id identify one group throughout a schema: §6.8,
"Name and id must uniquely identify a group type within a message
schema"; §6.9, "the same field or group ID may be used in multiple
messages, but each instance must represent the same type. Each of those
instances must match on both id and name attributes." A name or an id
standing for two different groups, their dimensions, members, offsets,
presences or versions differing, is an error.
`-tolerate-group-redefinitions` (`LoadOptions.TolerateGroupRedefinitions`)
accepts it, Binance's schema needing it for Orders, RelatedSym,
MDEntriesBids and MDEntriesAsks, and the generator names each such group
by its place (see [Generated code](#generated-code)).

The same rule holds for fields, `<data>` included (sbe.xsd types both
`fieldType`): §6.7, field `name`, "Name and id must uniquely identify a
field type within a message schema"; §6.9, "Attributes id and name must
uniquely identify a type within a message schema. This applies to fields
and groups." A field name standing for another id or encoding, or an id
for another name or encoding, is an error; the presence may differ, as a
FIX field's does between messages. `-tolerate-field-redefinitions`
(`LoadOptions.TolerateFieldRedefinitions`) accepts it, which the FIX
Trading Community's own FIXP session schema needs, naming fields `Code`
with ids 7, 12, 15 and 16 and giving id 8 to `Reason` and `Flow` (see
[docs/conformance.md](conformance.md#sbe)): `fixp/internal/codec` is
generated with it. So is Binance's, whose `ClOrdID`, `Symbol` and others
are `varString8` here and `optionalVarString8` there, and whose id 58 is
`Text` and `ErrorText`. The generated code does not depend on it.

A message header may have elements beside the six SBE lists (§5.2,
"The fields of the SBE message header are:"), of any type: SBE requires
its own to be unsigned (§5.2.1), which loading checks of `blockLength`,
`templateId`, `schemaId` and `version`, and of `numGroups` and
`numVarDataFields` when the header has them (a signed count would
otherwise pass for none, its counting unchecked), and says nothing of
others.
Binance's header adds `seqNum`, a `uint32`, and `sendingTime`, an
`int64` of microseconds.

`semanticType`, `description` and `deprecated` go into comments;
`minValue` and `maxValue` bound data lengths and group counts, and are
otherwise the application's to check.

## Tests

- The three examples of §9, decoded to the values they show and encoded
  back to their bytes. Their schema is repaired, in a copy saying so: its
  `id` is 91 as their bytes say, not 100, and its XML is made well-formed.
  Their interpretation tables disagree with their bytes for TransactTime
  and OrderID: the bytes win.
- Messages Real Logic's SBE 1.33.0 encodes
  (`internal/gen/sbe/testdata/reallogic`, its programs and commands
  pinned), for a richer schema with nested groups, data in groups,
  optionals of every kind, constants, sets, references, offsets, padding
  and counters, in both byte orders: decoded to the values its program
  set, and the same values encoded to the same bytes. The FIXP schema's
  every message checks `fixp/internal/codec` likewise, and so do the
  frames of the Silverflash spike, their SOFH length corrected and their
  padding ignored.
- Matrices beyond round trips: every null and boundary value, every older
  and newer version of the richer schema, truncation at every byte.
- The length decoding returns: whole messages, with bytes after them,
  root blocks and group entries longer than the schema's.
- Counts and a newer version: `testdata/older.xml` at version 1,
  generated four times, strictly (`older`), with `-assume-same-encodings`
  (`oldersame`), with `-tolerate-unfilled-counts` (`olderunfilled`) and
  with both (`oldersameunfilled`), and
  `testdata/newer.xml` at version 2 (`newer`, whose tests check them
  all), which adds a field, a group and a data field to a message and to
  its group's entries, the new groups holding groups and data of their
  own, and to another message a data field and a group of other
  encodings than version 1's. Strictly, version 2's messages are
  `ErrNewer`, a field added alone decoding; assuming the same encodings,
  they decode whole to the values encoded, every prefix failing, and
  those of other encodings are misread, as the assumption says. Counts
  of headers and dimensions, at versions 0 and 1: those of the version
  decode, others are `ErrMalformed`, and counts of 0 decode to the values
  encoded with `-tolerate-unfilled-counts`; in version 2, they are
  `ErrNewer` before what the decoder reads but with both flags. Real
  Logic's messages of the richer schema have their counts filled, by its
  program for the header and Legs' dimension, and need no flag. The
  richer schema and the §9 examples, without counts, check the strict
  rules: a newer version's tail left unread, `ErrNewer` where it could
  come before what the decoder reads.
- A schema whose header, dimensions and data lengths are 64 and 32 bits
  wide (`testdata/wide.xml`): their largest values, and those with the
  sign bit of an `int` set, decode as errors, never wrapping an `int`;
  data lengths are bounded both ways.
- The FIXP schema, and schemas with descriptions naming Go packages,
  generate code that type-checks.
- Binance's Spot FIX SBE schema (`internal/gen/sbe/binance`, the schema
  pinned to a commit of Binance's repository, generated with
  `-tolerate-foreign-metadata`, `-tolerate-undeclared-primitive-types`,
  `-tolerate-group-redefinitions` and `-tolerate-field-redefinitions`,
  each refused without): the sample
  Logon of Binance's FIX API documentation decoded to the values it
  lists, its 20-byte header's `seqNum` and `sendingTime` included, and
  encoded back to its bytes but for the header's version, the schema's;
  session, order, list and market data messages round-tripping through
  `AppendWithHeader`, `Decode` and `DecodeBody`, with optional data
  fields empty and set, null optionals of bare primitives and enums,
  groups with data and nested groups, NewOrderList's and ListStatus's
  differing Orders and their dimensions on the wire, and a version 0
  ExecutionReport without its version 1 field. Decimal mantissas and
  exponents are left raw, as the schema sends them.
- Fields and data whose name or id stands for another id, name or
  encoding refused, in one message or across messages, and accepted
  tolerated; the FIXP schema refused strictly. Headers with a signed,
  optional or array count refused. A group entry's member named
  `AppendWithHeader` accepted, a message's refused.
- Groups of one name differing, nested ones included, named by their
  places; groups of a name all the same sharing a type, inside a shared
  type too; two places whose names join into one Go name refused.
- Schemas that must be rejected, each with its error, by loading or by
  generation: misspelt attributes, elements out of place, other roots
  and namespaces, metadata of another namespace among them; and schemas
  whose metadata of another namespace loads, tolerated, as if it were
  not there.
- Fuzzing: arbitrary bytes decoded into every message of the test
  schemas, never panicking nor allocating beyond the bounds above, the
  length read within the input; what decodes encodes back to the same
  fields.
