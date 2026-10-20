package session

import "strconv"

// Encrypter seals and opens the SecureData (91) of a session's messages, for
// an EncryptMethod (98) other than 0, with keys the counterparties agreed
// beforehand, out of band, as FIX 4.4 has it: "The previously agreed upon
// encryption methodology is declared in the Logon message" (unverified: no
// FIX 4.4 text is pinned). FIX 4.x only: FIXT 1.1 deprecated it.
//
// Each side's Logon declares Method, the other's must declare the same: an
// acceptor refuses a Logon declaring another, or none, disconnecting without
// Logout (SL §4.3.1), and an initiator disconnects on such an answer. A
// session without Encrypter declares 0 and refuses any other (SL §4.3.3:
// "EncyptionMethod(98) must be present in the Logon(35=A) message and set to
// “0” when application layer encryption is not in use.").
//
// Seal and Open may run at the same time.
type Encrypter interface {
	// Method returns the EncryptMethod (98) the session's Logons declare: a
	// positive integer, which Config.Validate checks.
	Method() string
	// Seal appends to dst the sealed form of plain, the fields of a
	// message's body, tag=value<SOH> each, and returns the result.
	Seal(dst, plain []byte) ([]byte, error)
	// Open appends to dst the fields sealed holds, as Seal was given them,
	// and returns the result.
	Open(dst, sealed []byte) ([]byte, error)
}

// encryptMethod returns the EncryptMethod (98) our Logons declare, which the
// counterparty's must declare too: the Encrypter's, 0 (none) without one.
func (s *Session) encryptMethod() string {
	if s.cfg.Encrypter == nil {
		return "0"
	}
	return s.cfg.Encrypter.Method()
}

// validMethod reports whether v is an EncryptMethod (98) other than 0: a
// positive integer, as Logons compare it, without sign or leading zeros.
func validMethod(v string) bool {
	n, err := strconv.Atoi(v)
	return err == nil && n > 0 && strconv.Itoa(n) == v
}
