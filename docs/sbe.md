# fix2gen sbe

`fix2gen sbe` generates Go codecs for the messages of a Simple Binary
Encoding schema: [SBE 1.0](https://www.fixtrading.org/wp-content/uploads/download-manager-files/Simple_Binary_Encoding_V1.0_with_Errata_November_2020.pdf)
(FIX Technical Standard, with the November 2020 errata). Section numbers
below (§) are its own. FIXP's session messages are its first user
([docs/fixp.md](fixp.md)), venues' application schemas the next.

    fix2gen sbe -schema <xml-path> -pkg <package-name> [-out <dir>]

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
| message | a struct, its template ID as a constant, `AppendTo(dst) ([]byte, error)`, `Decode(b) error`, `DecodeBody(h, body) error` |
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
caller read, checking the template id. On error, the struct is left zero.
A decoded struct's `[]byte` fields alias the input: they are valid until
the input is changed or reused.

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
- A newer version may add fixed fields: they are skipped. It may also add
  groups and data fields; those cannot be found without the
  `numGroups`/`numVarDataFields` counts SBE 1.0 makes optional (§7.3.2),
  and fix2's codecs do not read them: a newer version's message decodes
  correctly only if it adds fixed fields alone. FIXP's schema is at
  version 0 and has no group.
- Values are not validated: an unknown enum value or a null in a required
  field decodes as it is, `Known()` and the application tell (§4.15,
  §7.4). Structure is (§5.6).
- Bytes after the message are ignored (see [FIXP](fixp.md#limits)).

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
- A schema whose header, dimensions and data lengths are 64 and 32 bits
  wide (`testdata/wide.xml`): their largest values, and those with the
  sign bit of an `int` set, decode as errors, never wrapping an `int`;
  data lengths are bounded both ways.
- The FIXP schema, and schemas with descriptions naming Go packages,
  generate code that type-checks.
- Schemas that must be rejected, each with its error, by loading or by
  generation.
- Fuzzing: arbitrary bytes decoded into every message of the test
  schemas, never panicking nor allocating beyond the bounds above; what
  decodes encodes back to the same fields.
