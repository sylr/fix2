package session

import (
 "context"
 "bytes"
 "sync"
 "testing"
 "time"
 fix2 "github.com/sylr/fix2"
 "github.com/sylr/fix2/session/transport"
 "github.com/sylr/fix2/session/transport/mem"
)

type reviewBlockedWrite struct { transport.Conn; entered, release chan struct{}; once sync.Once; mu sync.Mutex; writes [][]byte }
func (c *reviewBlockedWrite) Write(b []byte) (int,error) { c.once.Do(func(){close(c.entered); <-c.release}); c.mu.Lock();c.writes=append(c.writes,append([]byte(nil),b...));c.mu.Unlock(); return len(b),nil }
func reviewSession(t *testing.T, eager bool) *Session {
 s,err:=New(Config{ID:SessionID{BeginString:FIX44,SenderCompID:"A",TargetCompID:"B"}, Handler:BaseHandler{}, AckProbe:AckProbeNever})
 if err!=nil {t.Fatal(err)}
 if eager {s.cfg.AckProbe=AckProbeEager}
 return s
}
func TestReviewResetLeavesOldProbe(t *testing.T) {
 s:=reviewSession(t,true)
 a,b:=mem.Pipe(); defer b.Close()
 tc:=&reviewBlockedWrite{Conn:a,entered:make(chan struct{}),release:make(chan struct{})}
 c,err:=s.attach(tc,nil); if err!=nil {t.Fatal(err)}
 s.mu.Lock(); s.phase=phaseEstablished; s.queueLocked(&header{msgType:"D"},nil,false); s.mu.Unlock()
 <-tc.entered
 s.mu.Lock(); s.activeResetLocked(c,nil,Inbound{}); s.mu.Unlock()
 done:=make(chan struct{}); go func(){s.fenceReset(c);close(done)}()
 for {s.mu.Lock(); stopped:=c.writerStop;s.mu.Unlock(); if stopped {break};time.Sleep(time.Millisecond)}
 close(tc.release); <-done
 s.mu.Lock()
 // Original seq1 application write causes an eager TestRequest seq2 to be queued AFTER the reset drops pending,
 // and that old probe is not cleared before the reset's seq1 Logon.
 if s.st.NextSender!=2 {t.Fatalf("bad new next %d",s.st.NextSender)}
 s.mu.Unlock()
 deadline:=time.Now().Add(time.Second)
 for {tc.mu.Lock();n:=len(tc.writes);tc.mu.Unlock();if n>=2 {break};if time.Now().After(deadline){t.Fatal("no reset output")};time.Sleep(time.Millisecond)}
 tc.mu.Lock();out:=append([]byte(nil),tc.writes[1]...);tc.mu.Unlock()
 f:=fix2.NewFramer(bytes.NewReader(out),fix2.FramerOptions{})
 raw,err:=f.Next();if err!=nil {t.Fatal(err)}
 var m fix2.Message;if err:=fix2.ParseInto(&m,raw,fix2.DefaultProfile());err!=nil {t.Fatal(err)}
 if string(m.MsgType())!="A" || string(m.Get(34))!="1" {t.Errorf("first reset output is type=%s seq=%s, want Logon seq1",m.MsgType(),m.Get(34))}
 ctx,cancel:=context.WithCancel(context.Background());cancel();s.Stop(ctx)
}
func TestReviewDeferredResetHitsReplacement(t *testing.T) {
 s:=reviewSession(t,false);s.cfg.ResetOnDisconnect=true
 a,b:=mem.Pipe(); defer b.Close()
 tc:=&reviewBlockedWrite{Conn:a,entered:make(chan struct{}),release:make(chan struct{})}
 c,err:=s.attach(tc,nil);if err!=nil {t.Fatal(err)}
 s.mu.Lock();s.phase=phaseEstablished;s.queueLocked(&header{msgType:"D"},nil,false);s.mu.Unlock();<-tc.entered
 done:=make(chan struct{});go func(){c.run();close(done)}()
 b.Close()
 <-c.done()
 a2,b2:=mem.Pipe();defer b2.Close()
 c2,err:=s.attach(a2,nil);if err!=nil {t.Fatal(err)}
 s.mu.Lock();s.phase=phaseEstablished;s.queueLocked(&header{msgType:"D"},nil,false);old:=s.st.Generation;s.mu.Unlock()
 close(tc.release);<-done
 s.mu.Lock();gen:=s.st.Generation;next:=s.st.NextSender;current:=s.c==c2;s.mu.Unlock()
 if gen!=old || next!=3 {t.Errorf("old connection reset replacement: generation %d->%d NextSender=%d active=%v",old,gen,next,current)}
 ctx,cancel:=context.WithCancel(context.Background());cancel();s.Stop(ctx)
 _=fix2.Seconds
}
