# Real Logic cross-check

Messages encoded by Real Logic's SBE codecs, the independent encoder
fix2gen sbe's output is checked against: `rich.hex` and `bigrich.hex`,
the Order and Ping of `../rich.xml` and `../bigrich.xml`, `fixp.hex`,
every message of the FIXP schema (`fixp/internal/codec`), and
`binance.hex`, 54 messages of Binance's Spot FIX SBE schema
(`../../binance/spot-fixsbe-1_1.xml`), one per line, name then
hexadecimal, header included. `binance-sofh.hex` holds the same messages
framed as Binance sends them: the Simple Open Framing Header with both its
fields little-endian, then the message.

They come from `uk.co.real-logic:sbe-all:1.33.0` (Maven Central), its
Java codecs generated with `-Dsbe.target.language=Java` and Java 21
(`gen.sh.txt`, `binance.sh.txt`), and the programs `RichCheck.java.txt`,
`FixpCheck.java.txt` and `BinanceCheck.java.txt`, which set the values
fix2's tests check (`run.sh.txt`, `binance.sh.txt`). Real Logic does not
fill a group dimension's `numGroups` and `numVarDataFields`, nor the
header's: `RichCheck` sets them as fix2gen's codecs do. Nor does it fill
null values: `BinanceCheck` sets every field, the null ones to the null
value Real Logic's codecs give (`NULL_VAL`, `…NullValue()`). Real Logic
generates no codec of Binance's `sofh` composite, which no message names:
`BinanceCheck` writes that header itself.

Binance's messages cover the session messages (Logon, LogonAck,
Heartbeat, TestRequest, Reject, Logout, News), the order entry requests
and answers, and the market data messages, all 29 templates. Each of the
schema's 193 optional fields, data and groups' members included, is set
in some message and null in another (`TestRealLogicCoverage`, in
`internal/gen/sbe/binance`, enumerates them from the schema and checks
it): optional integers, enums, exponents' mantissas and timestamps,
optional strings empty; groups of
several entries and empty, nested groups (NewOrderList's and ListStatus's
Orders and their ListTriggeringInstructions), data of several lengths up
to a `varString8`'s 254 bytes and a `varString`'s 300. Every header
carries seqNum 4,000,000,000 + i and sendingTime 1719479845223000 +
1,001 i, i the message's index from 0. `contribs/binancefix` keeps a copy
of `binance.hex` and `binance-sofh.hex` in `testdata/reallogic`, which
`internal/gen/sbe/binance`'s tests check is the same.

To produce `rich.hex`, `bigrich.hex` and `fixp.hex` again: fetch the jar,
adjust the paths of `gen.sh.txt` and `run.sh.txt`, run them.

To produce `binance.hex` and `binance-sofh.hex` again, with Docker and no
local Java:

```sh
d=$(mktemp -d)
curl -fsSL -o "$d/sbe-all-1.33.0.jar" \
  https://repo1.maven.org/maven2/uk/co/real-logic/sbe-all/1.33.0/sbe-all-1.33.0.jar
# SHA-256 243d270ef962b297f860113bb4d75d4c550e3eceabe0079ddcff424a8f3f9449
cp internal/gen/sbe/binance/spot-fixsbe-1_1.xml "$d/"
cp internal/gen/sbe/testdata/reallogic/BinanceCheck.java.txt "$d/BinanceCheck.java"
cp internal/gen/sbe/testdata/reallogic/binance.sh.txt "$d/binance.sh"
docker run --rm -v "$d:/w" \
  eclipse-temurin:21-jdk@sha256:23cc5734ff46e776f51ce66eb9ee08444fd47caabec0a95b7eb6e94be2d3696d \
  bash /w/binance.sh
cp "$d/binance.hex" "$d/binance-sofh.hex" internal/gen/sbe/testdata/reallogic/
cp "$d/binance.hex" "$d/binance-sofh.hex" contribs/binancefix/testdata/reallogic/
```

The image is `eclipse-temurin:21-jdk` pinned by digest (Java 21). SbeTool
warns that MarketDataIncrementalTrade's group dimension counts with a
`uint32`, and generates it.
