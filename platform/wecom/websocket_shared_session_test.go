package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

func newSessionTestWSPlatform(t *testing.T, shared any) *WSPlatform {
	t.Helper()
	opts := map[string]any{
		"mode": "websocket", "bot_id": "bot", "bot_secret": "test-secret", "allow_from": "*",
	}
	if shared != nil {
		opts["share_session_in_channel"] = shared
	}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return p.(*WSPlatform)
}

func captureSessionTestCallback(t *testing.T, p *WSPlatform, chatID, chatType, userID, msgID string) *core.Message {
	t.Helper()
	captured := make(chan *core.Message, 1)
	p.handler = func(_ core.Platform, msg *core.Message) { captured <- msg }
	body := wsMsgCallbackBody{MsgID: msgID, ChatID: chatID, ChatType: chatType, MsgType: "text"}
	body.From.UserID = userID
	body.Text.Content = "hello"
	p.handleMsgCallback(wsCallbackFrame(t, "req_"+msgID, body))
	select {
	case msg := <-captured:
		return msg
	case <-time.After(time.Second):
		t.Fatal("handler was not called")
		return nil
	}
}

func TestHandleMsgCallback_ShareSessionInChannel(t *testing.T) {
	for _, shared := range []any{nil, false, true} {
		t.Run(fmt.Sprint(shared), func(t *testing.T) {
			p := newSessionTestWSPlatform(t, shared)
			for i, tc := range []struct{ chatID, chatType, userID string }{
				{"groupA", "group", "alice"},
				{"groupA", "group", "bob"},
				{"groupA", "group", "shared"},
				{"groupB", "group", "alice"},
				{"", "single", "alice"},
				{"", "single", "bob"},
				{"dmAlice", "single", "alice"},
				{"groupA", "single", "alice"},
				{"groupA", "single", "shared"},
				{"groupA", "", "alice"},
			} {
				msgID := fmt.Sprintf("msg%d", i)
				msg := captureSessionTestCallback(t, p, tc.chatID, tc.chatType, tc.userID, msgID)
				chatID := tc.chatID
				if chatID == "" {
					chatID = tc.userID
				}
				wantKey := "wecom:" + chatID + ":" + tc.userID
				if shared == true && tc.chatType == "group" {
					wantKey = "wecom:" + chatID + ":shared/group"
				}
				if msg.SessionKey != wantKey {
					t.Errorf("%+v: SessionKey = %q, want %q", tc, msg.SessionKey, wantKey)
				}
				if msg.UserID != tc.userID || msg.UserName != tc.userID || msg.Content != "hello" {
					t.Errorf("sender or content changed: %+v", msg)
				}
				wantCtx := wsReplyContext{reqID: "req_" + msgID, chatID: chatID, chatType: tc.chatType, userID: tc.userID}
				if msg.ReplyCtx != wantCtx {
					t.Errorf("ReplyCtx = %+v, want %+v", msg.ReplyCtx, wantCtx)
				}
			}
		})
	}
}

func TestHandleMsgCallback_SharedSessionChecksRealSenderAllowList(t *testing.T) {
	for _, tc := range []struct {
		allowFrom, userID string
		allowed           bool
	}{
		{"alice", "alice", true},
		{"alice", "bob", false},
		{"shared", "bob", false},
	} {
		t.Run(tc.allowFrom+"/"+tc.userID, func(t *testing.T) {
			p := newSessionTestWSPlatform(t, true)
			p.allowFrom = tc.allowFrom
			captured := make(chan *core.Message, 1)
			p.handler = func(_ core.Platform, msg *core.Message) { captured <- msg }
			body := wsMsgCallbackBody{MsgID: "auth", ChatID: "groupA", ChatType: "group", MsgType: "text"}
			body.From.UserID = tc.userID
			body.Text.Content = "/new"
			p.handleMsgCallback(wsCallbackFrame(t, "req_auth", body))
			select {
			case msg := <-captured:
				if !tc.allowed {
					t.Fatal("unauthorized sender reached the engine")
				}
				if msg.UserID != tc.userID || msg.SessionKey != "wecom:groupA:shared/group" {
					t.Fatalf("unexpected authorized message: %+v", msg)
				}
			case <-time.After(100 * time.Millisecond):
				if tc.allowed {
					t.Fatal("authorized sender did not reach the engine")
				}
			}
		})
	}
}

func TestSharedSession_PreservesSessionManagementAndRestore(t *testing.T) {
	p := newSessionTestWSPlatform(t, true)
	alice := captureSessionTestCallback(t, p, "groupA", "group", "alice", "alice")
	bob := captureSessionTestCallback(t, p, "groupA", "group", "bob", "bob")
	store := filepath.Join(t.TempDir(), "sessions.json")
	sm := core.NewSessionManager(store)
	legacy := sm.GetOrCreateActive("wecom:groupA:alice")
	legacy.SetAgentSessionID("legacy-claude-session", "claudecode")
	legacy.AddHistory("user", "legacy history")
	legacyBob := sm.GetOrCreateActive("wecom:groupA:bob")
	legacyShared := sm.GetOrCreateActive("wecom:groupA:shared")
	legacyShared.SetAgentSessionID("legacy-shared-user-session", "claudecode")
	first := sm.GetOrCreateActive(alice.SessionKey)
	if first == legacyShared {
		t.Fatal("shared group session reused the legacy session of user ID shared")
	}
	first.SetAgentSessionID("shared-claude-session", "claudecode")
	if sm.GetOrCreateActive(bob.SessionKey) != first {
		t.Fatal("group members did not resolve to the same session")
	}
	// Both members must contend for the existing session lock, not start
	// concurrent Claude turns on separate per-user sessions.
	gen, locked := first.TryLock()
	if !locked {
		t.Fatal("initial session lock failed")
	}
	if _, locked := sm.GetOrCreateActive(bob.SessionKey).TryLock(); locked {
		t.Fatal("second group member acquired a busy session")
	}
	first.Unlock(gen)
	second := sm.NewSession(bob.SessionKey, "second")
	if sm.GetOrCreateActive(alice.SessionKey) != second || len(sm.ListSessions(alice.SessionKey)) != 2 {
		t.Fatal("new session/list was not shared by the whole group")
	}
	if _, err := sm.SwitchSession(alice.SessionKey, first.ID); err != nil {
		t.Fatal(err)
	}
	if sm.GetOrCreateActive(bob.SessionKey) != first {
		t.Fatal("session switch did not apply to the other group member")
	}
	sm.Save()
	restored := core.NewSessionManager(store)
	if got := restored.GetOrCreateActive(bob.SessionKey); got.ID != first.ID || got.GetAgentSessionID() != "shared-claude-session" {
		t.Fatalf("shared session was not restored: %+v", got)
	}
	if got := restored.GetOrCreateActive("wecom:groupA:alice"); got.ID != legacy.ID || got.GetAgentSessionID() != "legacy-claude-session" || got.HistoryLen() != 1 {
		t.Fatalf("legacy session was changed: %+v", got)
	}
	if got := restored.GetOrCreateActive("wecom:groupA:bob"); got.ID != legacyBob.ID {
		t.Fatal("Bob's legacy session was changed")
	}
	if got := restored.GetOrCreateActive("wecom:groupA:shared"); got.ID != legacyShared.ID || got.GetAgentSessionID() != "legacy-shared-user-session" {
		t.Fatal("legacy session of user ID shared was changed")
	}
	if _, err := restored.SwitchSession("wecom:groupB:shared/group", first.ID); err == nil {
		t.Fatal("another group could switch to group A's session")
	}
}

func TestReconstructReplyCtx_SharedSession(t *testing.T) {
	// A saved shared key remains routable even after the option is disabled.
	for _, shared := range []bool{false, true} {
		p := newSessionTestWSPlatform(t, shared)
		ctx, err := p.ReconstructReplyCtx("wecom:groupA:shared/group")
		if err != nil {
			t.Fatal(err)
		}
		want := wsReplyContext{chatID: "groupA", chatType: "group"}
		if ctx != want {
			t.Fatalf("context = %+v, want %+v; shared is not a sender identity", ctx, want)
		}
		legacy, err := p.ReconstructReplyCtx("wecom:groupA:alice")
		if err != nil || legacy != (wsReplyContext{chatID: "groupA", userID: "alice"}) {
			t.Fatalf("legacy context changed: %+v, %v", legacy, err)
		}
		legacy, err = p.ReconstructReplyCtx("wecom:groupA:shared")
		if err != nil || legacy != (wsReplyContext{chatID: "groupA", userID: "shared"}) {
			t.Fatalf("legacy user shared was treated as a group marker: %+v, %v", legacy, err)
		}
	}
}

func TestHandleMsgCallback_SharedSessionMediaAndVoice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="attachment.txt"`)
		_, _ = w.Write([]byte("test attachment"))
	}))
	defer server.Close()
	for _, tc := range []struct {
		msgType       string
		images, files int
	}{
		{"voice", 0, 0},
		{"image", 1, 0},
		{"file", 0, 1},
		{"mixed", 1, 1},
	} {
		t.Run(tc.msgType, func(t *testing.T) {
			p := newSessionTestWSPlatform(t, true)
			captured := make(chan *core.Message, 1)
			p.handler = func(_ core.Platform, msg *core.Message) { captured <- msg }
			media := map[string]any{"url": server.URL + "/media"}
			body := map[string]any{
				"msgid": tc.msgType, "chatid": "groupA", "chattype": "group",
				"from": map[string]string{"userid": "bob"}, "msgtype": tc.msgType,
				tc.msgType: media,
			}
			if tc.msgType == "voice" {
				body["voice"] = map[string]string{"content": "voice transcription"}
			} else if tc.msgType == "mixed" {
				body["mixed"] = map[string]any{"msg_item": []any{
					map[string]any{"msgtype": "text", "text": map[string]string{"content": "inspect attachments"}},
					map[string]any{"msgtype": "image", "image": media},
					map[string]any{"msgtype": "file", "file": media},
				}}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			p.handleMsgCallback(wsFrame{Headers: wsFrameHeaders{ReqID: "req_media"}, Body: raw})
			select {
			case msg := <-captured:
				if msg.SessionKey != "wecom:groupA:shared/group" || msg.UserID != "bob" || msg.UserName != "bob" {
					t.Fatalf("media sender or session changed: %+v", msg)
				}
				if len(msg.Images) != tc.images || len(msg.Files) != tc.files || msg.FromVoice != (tc.msgType == "voice") {
					t.Fatalf("media payload changed: %+v", msg)
				}
				if tc.msgType == "voice" && msg.Content != "voice transcription" {
					t.Fatalf("voice content = %q", msg.Content)
				}
				if msg.ReplyCtx != (wsReplyContext{reqID: "req_media", chatID: "groupA", chatType: "group", userID: "bob"}) {
					t.Fatalf("media reply context changed: %+v", msg.ReplyCtx)
				}
			case <-time.After(time.Second):
				t.Fatal("media was not dispatched")
			}
		})
	}
}

func TestSharedSession_ReplyAndProactiveSendRouting(t *testing.T) {
	frames := make(chan wsFrame, 8)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			frames <- frame
			if err := conn.WriteJSON(map[string]any{"headers": frame.Headers, "errcode": 0}); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := newSessionTestWSPlatform(t, true)
	p.conn = conn
	go func() {
		for {
			var frame wsFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			p.handleFrame(frame)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, user := range []string{"alice", "bob"} {
		msg := captureSessionTestCallback(t, p, "groupA", "group", user, user)
		if err := p.Reply(ctx, msg.ReplyCtx, "reply to "+user); err != nil {
			t.Fatal(err)
		}
		select {
		case frame := <-frames:
			if frame.Cmd != "aibot_respond_msg" || frame.Headers.ReqID != "req_"+user {
				t.Fatalf("reply lost original callback routing: %+v", frame)
			}
		case <-ctx.Done():
			t.Fatal("missing reply frame")
		}
	}
	for _, tc := range []struct{ key, chatID string }{
		{"wecom:groupA:shared/group", "groupA"},
		{"wecom:groupB:shared/group", "groupB"},
		{"wecom:groupA:shared", "groupA"},
		{"wecom:groupA:alice", "groupA"},
		{"wecom:alice:alice", "alice"},
	} {
		rctx, err := p.ReconstructReplyCtx(tc.key)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Send(ctx, rctx, "proactive"); err != nil {
			t.Fatal(err)
		}
		select {
		case frame := <-frames:
			var body struct {
				ChatID string `json:"chatid"`
			}
			if err := json.Unmarshal(frame.Body, &body); err != nil {
				t.Fatal(err)
			}
			if frame.Cmd != "aibot_send_msg" || body.ChatID != tc.chatID {
				t.Fatalf("%s routed to %q with cmd %q", tc.key, body.ChatID, frame.Cmd)
			}
		case <-ctx.Done():
			t.Fatal("missing proactive send frame")
		}
	}
}
