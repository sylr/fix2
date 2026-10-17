# Real Logic cross-check

Messages encoded by Real Logic's SBE codecs, the independent encoder
fix2gen sbe's output is checked against: `rich.hex` and `bigrich.hex`,
the Order and Ping of `../rich.xml` and `../bigrich.xml`, and `fixp.hex`,
every message of the FIXP schema (`fixp/internal/codec`), one per line,
name then hexadecimal, header included.

They come from `uk.co.real-logic:sbe-all:1.33.0` (Maven Central), its
Java codecs generated with `-Dsbe.target.language=Java` and Java 21
(`gen.sh.txt`), and the programs `RichCheck.java.txt` and
`FixpCheck.java.txt`, which set the values fix2's tests check
(`run.sh.txt`). Real Logic does not fill a group dimension's `numGroups`
and `numVarDataFields`, nor the header's: `RichCheck` sets them as
fix2gen's codecs do.

Binance's Spot FIX SBE schema has its own cross-check, with the codecs
generated from it: `contribs/binancefix/testdata/reallogic`, whose README
tells how its fixtures are made. fix2 keeps the schema as its generator's
test data (`../binance`).

To produce `rich.hex`, `bigrich.hex` and `fixp.hex` again: fetch the jar,
adjust the paths of `gen.sh.txt` and `run.sh.txt`, run them.
