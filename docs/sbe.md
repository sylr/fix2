# fix2gen sbe

`fix2gen sbe` generates Go codecs for the messages of a Simple Binary
Encoding schema: [SBE 1.0](https://www.fixtrading.org/wp-content/uploads/download-manager-files/Simple_Binary_Encoding_V1.0_with_Errata_November_2020.pdf)
(FIX Technical Standard, with the November 2020 errata). Section numbers
below (§) are its own. FIXP's session messages are its first user
([docs/fixp.md](fixp.md)), venues' application schemas the next.

    fix2gen sbe -schema <xml-path> -pkg <package-name> [-out <dir>]
        [-tolerate-foreign-metadata] [-tolerate-unfilled-counts] [-assume-same-encodings]

By default it follows SBE 1.0 strictly. Three flags, off by default, each
change what it accepts or what the decoders it generates do, and are
named in the generated package's doc comment:

| Flag | Does | Kind |
|---|---|---|
| `-tolerate-foreign-metadata` | loads a schema holding elements and attributes of other namespaces, and ignores them (see [Schemas](#schemas)) | a toleration: sbe.xsd, normative, allows none |
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
| `<group>` | a slice of entry structs, one type per group name: a name defines one group throughout a schema (§6.9), which loading checks |
| `<data>` | `[]byte`, decoded as a subslice of the input |
| message | a struct, its template ID as a constant, `AppendTo(dst) ([]byte, error)`, `Decode(b) (int, error)`, `DecodeBody(h, body) (int, error)` |
| `messageHeader` | `MessageHeader`, `ReadHeader(b) (MessageHeader, []byte, error)` |

Names are the schema's, exported: `ClOrdID`, `SideEnum`, `SideEnumBuy`.
Two schema names giving one Go name, a member named as a generated
method (`AppendTo`, `Decode`, `DecodeBody`, `TemplateID`, a constant's or
`XString`'s method), or two messages giving one file name (`FooBar` and
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
| `presence` required, optional (with `nullValue`) and constant (a value or `valueRef`) | `nullValue` on a required or constant type; a constant without a value |
| `<composite>` with `<type>`, `<enum>`, `<set>`, `<composite>` and `<ref>` | a composite without elements; a reference cycle |
| `<enum>` over `char` or an unsigned type; `<set>` over an unsigned type | other encodings |
| `offset`, `blockLength` | an offset overlapping the previous member; a `blockLength` under its fields' |
| `sinceVersion`, `deprecated` | either above the schema's version |
| `headerType`, `dimensionType` of the schema's | a header without unsigned `blockLength`, `templateId`, `schemaId`, `version`; a dimension without `blockLength` and `numInGroup` |
| both byte orders | `xi:include` |
| `minValue`, `maxValue`, `nullValue` | one its type cannot represent; a data length's or a count's `minValue` above its `maxValue` |
| | a group or data field before a field, a group after data (§5.5); duplicate names or ids; a group name defining two different groups, its members' versions included; ids, versions or block lengths the header or dimension cannot hold |

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
- Schemas that must be rejected, each with its error, by loading or by
  generation: misspelt attributes, elements out of place, other roots
  and namespaces, metadata of another namespace among them; and schemas
  whose metadata of another namespace loads, tolerated, as if it were
  not there.
- Fuzzing: arbitrary bytes decoded into every message of the test
  schemas, never panicking nor allocating beyond the bounds above, the
  length read within the input; what decodes encodes back to the same
  fields.
