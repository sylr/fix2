# Real Logic cross-check

Messages encoded by Real Logic's SBE codecs, the independent encoder
binancefix's generated codecs are checked against: `binance.hex`, 54
messages of Binance's Spot FIX SBE schema
(`../../internal/fixsbe/spot-fixsbe-1_1.xml`), one per line, name then
hexadecimal, header included. `binance-sofh.hex` holds the same messages
framed as Binance sends them: the Simple Open Framing Header with both its
fields little-endian, then the message.

They come from `uk.co.real-logic:sbe-all:1.33.0` (Maven Central), its
Java codecs generated with `-Dsbe.target.language=Java` and Java 21
(`binance.sh.txt`), and the program `BinanceCheck.java.txt`, which sets
the values the tests check. Real Logic does not fill null values:
`BinanceCheck` sets every field, the null ones to the null value Real
Logic's codecs give (`NULL_VAL`, `…NullValue()`). Real Logic generates no
codec of Binance's `sofh` composite, which no message names:
`BinanceCheck` writes that header itself.

The messages cover the session messages (Logon, LogonAck, Heartbeat,
TestRequest, Reject, Logout, News), the order entry requests and answers,
and the market data messages, all 29 templates. Each of the schema's 193
optional fields, data and groups' members included, is set in some
message and null in another (`TestRealLogicCoverage`, in
`internal/fixsbe`, enumerates them from the schema and checks it):
optional integers, enums, exponents' mantissas and timestamps, optional
strings empty; groups of several entries and empty, nested groups
(NewOrderList's and ListStatus's Orders and their
ListTriggeringInstructions), data of several lengths up to a
`varString8`'s 254 bytes and a `varString`'s 300. Every header carries
seqNum 4,000,000,000 + i and sendingTime 1719479845223000 + 1,001 i, i
the message's index from 0.

`internal/fixsbe`'s tests decode every message to the values
`BinanceCheck` sets, field by field, and encode them back to Real Logic's
bytes; the client's (`../../reallogic_test.go`) serve them to a `Client`.

To produce `binance.hex` and `binance-sofh.hex` again, with Docker and no
local Java, from the repository's root:

```sh
d=$(mktemp -d)
curl -fsSL -o "$d/sbe-all-1.33.0.jar" \
  https://repo1.maven.org/maven2/uk/co/real-logic/sbe-all/1.33.0/sbe-all-1.33.0.jar
# SHA-256 243d270ef962b297f860113bb4d75d4c550e3eceabe0079ddcff424a8f3f9449
cp contribs/binancefix/internal/fixsbe/spot-fixsbe-1_1.xml "$d/"
cp contribs/binancefix/testdata/reallogic/BinanceCheck.java.txt "$d/BinanceCheck.java"
cp contribs/binancefix/testdata/reallogic/binance.sh.txt "$d/binance.sh"
docker run --rm -v "$d:/w" \
  eclipse-temurin:21-jdk@sha256:23cc5734ff46e776f51ce66eb9ee08444fd47caabec0a95b7eb6e94be2d3696d \
  bash /w/binance.sh
cp "$d/binance.hex" "$d/binance-sofh.hex" contribs/binancefix/testdata/reallogic/
```

The image is `eclipse-temurin:21-jdk` pinned by digest (Java 21). SbeTool
warns that MarketDataIncrementalTrade's group dimension counts with a
`uint32`, and generates it.
