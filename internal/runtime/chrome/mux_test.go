package chrome

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/runtime"
)

// fakeBrowser is the browser end of a debugging pipe. It opens a session
// for every Target.attachToBrowserTarget and records everything else.
type fakeBrowser struct {
	t        *testing.T
	out      io.WriteCloser
	mu       sync.Mutex
	sessions int
	received []map[string]any
	arrived  chan struct{}
}

func newFakeBrowser(t *testing.T) (*fakeBrowser, *mux) {
	t.Helper()
	commandsOut, commandsIn := io.Pipe()
	messagesOut, messagesIn := io.Pipe()
	b := &fakeBrowser{t: t, out: messagesIn, arrived: make(chan struct{}, 64)}
	go b.serve(commandsOut)
	m := newMux(commandsIn, messagesOut)
	t.Cleanup(func() {
		m.Close()
		_ = messagesIn.Close()
	})
	return b, m
}

func (b *fakeBrowser) serve(commands io.Reader) {
	reader := bufio.NewReader(commands)
	for {
		raw, err := reader.ReadBytes(0)
		if err != nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(raw[:len(raw)-1], &msg); err != nil {
			b.t.Errorf("the browser received invalid JSON: %q", raw)
			continue
		}
		if msg["method"] == methodAttachToBrowser && msg["sessionId"] == nil {
			b.mu.Lock()
			b.sessions++
			id := fmt.Sprintf("browser-%d", b.sessions)
			b.mu.Unlock()
			b.send(fmt.Sprintf(`{"id":%v,"result":{"sessionId":%q}}`, msg["id"], id))
			continue
		}
		b.mu.Lock()
		b.received = append(b.received, msg)
		b.mu.Unlock()
		b.arrived <- struct{}{}
	}
}

func (b *fakeBrowser) send(msg string) {
	if _, err := b.out.Write(append([]byte(msg), 0)); err != nil {
		b.t.Logf("browser write: %v", err)
	}
}

// next returns the next message the browser received from a client.
func (b *fakeBrowser) next() map[string]any {
	b.t.Helper()
	select {
	case <-b.arrived:
	case <-time.After(5 * time.Second):
		b.t.Fatal("the browser received nothing")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	msg := b.received[0]
	b.received = b.received[1:]
	return msg
}

func mustConnect(t *testing.T, m *mux) runtime.CDPConn {
	t.Helper()
	conn, err := m.Connect(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustRead(t *testing.T, conn runtime.CDPConn) map[string]any {
	t.Helper()
	raw, err := conn.Read(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("client received invalid JSON: %q", raw)
	}
	return msg
}

func expectNothing(t *testing.T, conn runtime.CDPConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if raw, err := conn.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected message %q (%v)", raw, err)
	}
}

func TestMuxGivesEachClientItsOwnBrowserSession(t *testing.T) {
	browser, m := newFakeBrowser(t)
	a, b := mustConnect(t, m), mustConnect(t, m)
	ctx := testContext(t)

	// Both clients use the same command ID, as independent clients do.
	if err := a.Write(ctx, []byte(`{"id":1,"method":"Browser.getVersion"}`)); err != nil {
		t.Fatal(err)
	}
	fromA := browser.next()
	if err := b.Write(ctx, []byte(`{"id":1,"method":"Target.getTargets","params":{"x":[1,2]}}`)); err != nil {
		t.Fatal(err)
	}
	fromB := browser.next()
	if fromA["sessionId"] != "browser-1" || fromB["sessionId"] != "browser-2" {
		t.Fatalf("commands were not scoped to their client's session: %v / %v", fromA, fromB)
	}
	if fromB["method"] != "Target.getTargets" || fmt.Sprint(fromB["params"]) != "map[x:[1 2]]" {
		t.Fatalf("the command was altered: %v", fromB)
	}

	// Answers come back to the right client, without the session.
	browser.send(`{"id":1,"sessionId":"browser-2","result":{"who":"b"}}`)
	browser.send(`{"id":1,"sessionId":"browser-1","result":{"who":"a"}}`)
	gotA, gotB := mustRead(t, a), mustRead(t, b)
	if fmt.Sprint(gotA) != "map[id:1 result:map[who:a]]" || fmt.Sprint(gotB) != "map[id:1 result:map[who:b]]" {
		t.Fatalf("answers: a=%v b=%v", gotA, gotB)
	}
}

func TestMuxRoutesAttachedSessionsToTheirClient(t *testing.T) {
	browser, m := newFakeBrowser(t)
	a, b := mustConnect(t, m), mustConnect(t, m)
	ctx := testContext(t)

	// A page is auto-attached for client A, and a worker beneath the page.
	browser.send(`{"method":"Target.attachedToTarget","sessionId":"browser-1","params":{"sessionId":"page-1","targetInfo":{}}}`)
	if got := mustRead(t, a); got["method"] != eventAttached || got["sessionId"] != nil {
		t.Fatalf("attach event = %v", got)
	}
	browser.send(`{"method":"Target.attachedToTarget","sessionId":"page-1","params":{"sessionId":"worker-1"}}`)
	browser.send(`{"method":"Runtime.consoleAPICalled","sessionId":"worker-1","params":{}}`)
	if got := mustRead(t, a); got["sessionId"] != "page-1" {
		t.Fatalf("page event = %v", got)
	}
	if got := mustRead(t, a); got["sessionId"] != "worker-1" || got["method"] != "Runtime.consoleAPICalled" {
		t.Fatalf("worker event = %v", got)
	}
	expectNothing(t, b)

	// Messages that name a session pass through untouched.
	const command = `{"id":7,"method":"Page.navigate","sessionId":"page-1","params":{"url":"about:blank"}}`
	if err := a.Write(ctx, []byte(command)); err != nil {
		t.Fatal(err)
	}
	if got := browser.next(); got["sessionId"] != "page-1" || got["id"] != float64(7) {
		t.Fatalf("browser received %v", got)
	}

	// A session attached explicitly is learned from the command's answer.
	browser.send(`{"id":8,"sessionId":"browser-2","result":{"sessionId":"page-2"}}`)
	mustRead(t, b)
	browser.send(`{"method":"Page.loadEventFired","sessionId":"page-2","params":{}}`)
	if got := mustRead(t, b); got["sessionId"] != "page-2" {
		t.Fatalf("event on an explicitly attached session = %v", got)
	}

	// After a detach the session's messages go nowhere.
	browser.send(`{"method":"Target.detachedFromTarget","sessionId":"browser-1","params":{"sessionId":"page-1"}}`)
	mustRead(t, a)
	browser.send(`{"method":"Page.loadEventFired","sessionId":"page-1","params":{}}`)
	expectNothing(t, a)
}

func TestMuxDetachesAClientThatLeaves(t *testing.T) {
	browser, m := newFakeBrowser(t)
	a, b := mustConnect(t, m), mustConnect(t, m)
	browser.send(`{"method":"Target.attachedToTarget","sessionId":"browser-1","params":{"sessionId":"page-1"}}`)
	mustRead(t, a)

	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	_ = a.Close() // closing twice is harmless
	detach := browser.next()
	if detach["method"] != methodDetach || detach["sessionId"] != nil ||
		fmt.Sprint(detach["params"]) != "map[sessionId:browser-1]" {
		t.Fatalf("the browser was not told to drop the client's session: %v", detach)
	}
	if _, err := a.Read(testContext(t)); !errors.Is(err, errConnClosed) {
		t.Fatalf("read after close: %v", err)
	}
	if err := a.Write(testContext(t), []byte(`{"id":1}`)); !errors.Is(err, errConnClosed) {
		t.Fatalf("write after close: %v", err)
	}
	// Late messages for the departed client are dropped; others carry on.
	browser.send(`{"method":"Page.loadEventFired","sessionId":"page-1","params":{}}`)
	browser.send(`{"id":3,"sessionId":"browser-2","result":{}}`)
	if got := mustRead(t, b); got["id"] != float64(3) {
		t.Fatalf("b received %v", got)
	}
}

func TestMuxRejectsMessagesThatAreNotCDP(t *testing.T) {
	_, m := newFakeBrowser(t)
	conn := mustConnect(t, m)
	for _, msg := range []string{"", "not json", `[1,2]`, `"text"`, "{\"id\":1}\x00{\"id\":2}", `{"id":1,"sessionId":5}`} {
		if err := conn.Write(testContext(t), []byte(msg)); !errors.Is(err, runtime.ErrInvalidMessage) {
			t.Errorf("%q: err = %v, want ErrInvalidMessage", msg, err)
		}
	}
}

func TestMuxDeliversQueuedMessagesBeforeReportingABrowserExit(t *testing.T) {
	browser, m := newFakeBrowser(t)
	conn := mustConnect(t, m)
	browser.send(`{"id":1,"sessionId":"browser-1","result":{}}`)
	// Wait until the mux has routed the message before the browser exits.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := conn.(*muxConn)
		c.mu.Lock()
		queued := len(c.queue)
		c.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the message never arrived")
		}
		time.Sleep(time.Millisecond)
	}
	_ = browser.out.Close()
	<-m.Done()
	if got := mustRead(t, conn); got["id"] != float64(1) {
		t.Fatalf("queued message = %v", got)
	}
	if _, err := conn.Read(testContext(t)); !errors.Is(err, errBrowserGone) {
		t.Fatalf("read after the browser exited: %v", err)
	}
	if _, err := m.Connect(testContext(t)); !errors.Is(err, errBrowserGone) {
		t.Fatalf("connect after the browser exited: %v", err)
	}
	if _, err := m.call(testContext(t), "Browser.getVersion", nil); !errors.Is(err, errBrowserGone) {
		t.Fatalf("call after the browser exited: %v", err)
	}
}

func TestMuxDisconnectsAClientThatStopsReading(t *testing.T) {
	browser, m := newFakeBrowser(t)
	m.queueLimit = 1 << 20
	slow, healthy := mustConnect(t, m), mustConnect(t, m)
	chunk := `{"method":"Tracing.dataCollected","sessionId":"browser-1","params":{"value":"` +
		strings.Repeat("x", 128<<10) + `"}}`
	for sent := 0; sent <= m.queueLimit; sent += len(chunk) {
		browser.send(chunk)
	}
	browser.send(`{"id":1,"sessionId":"browser-2","result":{}}`)
	if got := mustRead(t, healthy); got["id"] != float64(1) {
		t.Fatalf("the healthy client was held up: %v", got)
	}
	if _, err := slow.Read(testContext(t)); !errors.Is(err, errSlowConsumer) {
		t.Fatalf("slow client: %v", err)
	}
}

func TestMuxRootCalls(t *testing.T) {
	browser, m := newFakeBrowser(t)
	done := make(chan error, 1)
	go func() {
		_, err := m.call(context.Background(), "Browser.close", nil)
		done <- err
	}()
	request := browser.next()
	if request["method"] != "Browser.close" || request["sessionId"] != nil {
		t.Fatalf("root call = %v", request)
	}
	browser.send(fmt.Sprintf(`{"id":%v,"error":{"code":-1,"message":"nope"}}`, request["id"]))
	if err := <-done; err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	// A call abandoned by its caller does not wait for the browser.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := m.call(ctx, "Browser.getVersion", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// Events and stray answers on the root connection are ignored.
	browser.send(`{"method":"Target.targetCreated","params":{}}`)
	browser.send(`{"id":999,"result":{}}`)
	browser.send(`not json`)
	browser.send(`{"id":1,"sessionId":"unknown","result":{}}`)
	mustConnect(t, m)
}
