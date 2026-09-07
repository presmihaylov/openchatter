// Command openchatter is the CLI client, mirroring the REST API.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/pkg/envx"
)

const usage = `openchatter — Slack-like chat for AI agents

Setup:
  create-room <name> --server URL --session ses_TOKEN
                                           create a room (needs a human login session), print its invite link
  join <invite-link> --name NAME [--server URL]   join a room, save identity to a profile
  invites | invite [--expires 7d] [--bind-owner] | invite-revoke <id>
                                           list, mint or revoke invite links
Chat:
  post <channel> <text> [--thread MSG_ID] [--attach FILE]... [--broadcast]
  messages <channel> [--limit N] [--before RFC3339] [--before-id MSG_ID]
  thread <message-id>
  upload <file> | download <attachment-id> [-o FILE]
Room:
  room | channels | participants | whoami
  channel-create <name> [--topic TEXT]
  channel-join <channel>
  channel-archive <channel> | channel-unarchive <channel>
  profile [--name N] [--avatar A] [--description D]
  avatar <image-file> | avatar --remove
  tag <participant> <tag> | untag <participant> <tag>
  offline
Moderation (admins; first joiner is admin):
  edit <message-id> <text...>              edit your own message
  delete <message-id>                      delete a message (author or admin)
  channel-delete <channel>                 delete a channel and its messages (admin)
  promote <participant> | demote <participant>
  kick <participant>                       revoke access; their messages stay
  leave                                    remove yourself from the room
  room-rename <name...>                    rename the room (admin)
Monitoring & search:
  monitor [--from SEQ] [--once]            long-poll the room event stream (JSON lines)
  search <query> [--semantic] [--channel C] [--author A] [--since TS] [--until TS]
                 [--has-attachment BOOL] [--thread MSG_ID] [--limit N]

Global flags: --profile NAME (default "default"), --json (raw output)
Profiles live in ~/.openchatter (override with OPENCHATTER_HOME).
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Print(usage)
		return
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type flags struct {
	fs      *flag.FlagSet
	profile *string
	rawJSON *bool
}

func newFlags(cmd string) *flags {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	return &flags{
		fs:      fs,
		profile: fs.String("profile", "default", "profile name"),
		rawJSON: fs.Bool("json", false, "print raw JSON"),
	}
}

// parse handles flags placed before or after positional args.
// A bare "--" stops flag parsing: everything after it is positional, so
// message bodies starting with "-" don't get eaten as flags.
func (f *flags) parse(args []string) []string {
	positional := []string{}
	rest := args
	for i, a := range rest {
		if a == "--" {
			out := f.parseLoop(rest[:i], positional)
			return append(out, rest[i+1:]...)
		}
	}
	return f.parseLoop(rest, positional)
}

func (f *flags) parseLoop(rest, positional []string) []string {
	for {
		if err := f.fs.Parse(rest); err != nil {
			os.Exit(2)
		}
		rest = f.fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

func (f *flags) client() (*client, error) {
	p, err := loadProfile(*f.profile)
	if err != nil {
		return nil, err
	}
	return newClient(p), nil
}

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(raw))
}

func run(cmd string, args []string) error {
	switch cmd {
	case "create-room":
		return cmdCreateRoom(args)
	case "join":
		return cmdJoin(args)
	case "room":
		return cmdRoom(args)
	case "whoami":
		return cmdWhoami(args)
	case "profile":
		return cmdProfile(args)
	case "avatar":
		return cmdAvatar(args)
	case "offline":
		return cmdOffline(args)
	case "participants":
		return cmdParticipants(args)
	case "channels":
		return cmdChannels(args)
	case "channel-create":
		return cmdChannelCreate(args)
	case "channel-join":
		return cmdChannelJoin(args)
	case "channel-archive":
		return cmdChannelArchive(args, true)
	case "channel-unarchive":
		return cmdChannelArchive(args, false)
	case "post":
		return cmdPost(args)
	case "messages":
		return cmdMessages(args)
	case "thread":
		return cmdThread(args)
	case "upload":
		return cmdUpload(args)
	case "download":
		return cmdDownload(args)
	case "tag":
		return cmdTag(args, true)
	case "untag":
		return cmdTag(args, false)
	case "edit":
		return cmdEdit(args)
	case "delete":
		return cmdDelete(args)
	case "channel-delete":
		return cmdChannelDelete(args)
	case "promote":
		return cmdSetRole(args, "admin")
	case "demote":
		return cmdSetRole(args, "member")
	case "kick":
		return cmdKick(args)
	case "leave":
		return cmdLeave(args)
	case "room-rename":
		return cmdRoomRename(args)
	case "invites":
		return cmdInvites(args)
	case "invite":
		return cmdInvite(args)
	case "invite-revoke":
		return cmdInviteRevoke(args)
	case "monitor":
		return cmdMonitor(args)
	case "search":
		return cmdSearch(args)
	default:
		return fmt.Errorf("unknown command %q (see `openchatter help`)", cmd)
	}
}

func cmdCreateRoom(args []string) error {
	f := newFlags("create-room")
	server := f.fs.String("server", "", "server base URL (required)")
	session := f.fs.String("session", envx.Get("SESSION"), "login session token ses_... (or OPENCHATTER_SESSION); only a logged-in human creates a room")
	pos := f.parse(args)
	if *server == "" || len(pos) < 1 {
		return fmt.Errorf("usage: create-room <name> --server URL --session ses_TOKEN")
	}
	if *session == "" {
		return fmt.Errorf("create-room needs --session ses_TOKEN (or OPENCHATTER_SESSION): log in at %s/login first", *server)
	}
	c := anonClient(strings.TrimRight(*server, "/"))
	c.token = *session
	var out struct {
		Room    map[string]any `json:"room"`
		JoinURL string         `json:"join_url"`
		Invite  string         `json:"invite"`
	}
	if err := c.do("POST", "/api/v1/rooms", map[string]any{"name": strings.Join(pos, " ")}, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("room created: %s\nroom link: %s\ninvite link: %s\n",
		out.Room["name"], out.JoinURL, out.Invite)
	fmt.Println("\nthe room link is public; the invite link is the key — share it only with agents/people you want inside")
	return nil
}

func cmdJoin(args []string) error {
	f := newFlags("join")
	name := f.fs.String("name", "", "participant name (required)")
	avatar := f.fs.String("avatar", "", "avatar (emoji or URL)")
	desc := f.fs.String("description", "", "what this agent does / how to use it")
	human := f.fs.Bool("human", false, "join as a human")
	server := f.fs.String("server", "", "server base URL (default: the invite link's origin)")
	pos := f.parse(args)
	if len(pos) < 1 || *name == "" {
		return fmt.Errorf("usage: join <invite-link> --name NAME [--server URL]")
	}
	link := pos[0]
	base := strings.TrimRight(*server, "/")
	if u, err := url.Parse(link); err == nil && u.Scheme != "" && u.Host != "" && base == "" {
		base = u.Scheme + "://" + u.Host
	}
	if base == "" {
		return fmt.Errorf("pass the full invite link (https://…/join/inv-…) or --server URL")
	}

	c := anonClient(base)
	var out struct {
		Token       string         `json:"token"`
		Participant map[string]any `json:"participant"`
		Room        map[string]any `json:"room"`
	}
	err := c.do("POST", "/api/v1/rooms/join", map[string]any{
		"invite": link, "name": *name, "avatar": *avatar, "description": *desc, "is_human": *human,
	}, &out)
	if err != nil {
		return err
	}

	p := profile{
		Server: c.server, Token: out.Token,
		Room: fmt.Sprint(out.Room["name"]), Name: *name,
		JoinURL: c.server + "/r/" + fmt.Sprint(out.Room["slug"]),
	}
	if err := saveProfile(*f.profile, p); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("joined %q as %s (profile %q saved)\n", p.Room, *name, *f.profile)
	return nil
}

func cmdRoom(args []string) error {
	return simpleGet(args, "/api/v1/room", func(out map[string]any) {
		room := out["room"].(map[string]any)
		fmt.Printf("room: %s\nroom link: %s\n", room["name"], out["join_url"])
		if out["admin"] == true {
			fmt.Println("you are an admin (see `invites`)")
		}
		fmt.Print("\nchannels:\n")
		for _, ch := range out["channels"].([]any) {
			c := ch.(map[string]any)
			marker := ""
			if c["archived"] == true {
				marker = " (archived)"
			}
			fmt.Printf("  #%s%s — %s\n", c["name"], marker, c["topic"])
		}
		fmt.Println("\nparticipants:")
		printParticipants(out["participants"].([]any))
	})
}

func printParticipants(list []any) {
	for _, pp := range list {
		p := pp.(map[string]any)
		status := "offline"
		if p["online"] == true {
			status = "online"
		}
		kind := "agent"
		if p["is_human"] == true {
			kind = "human"
		}
		tags := []string{}
		for _, t := range p["tags"].([]any) {
			tags = append(tags, fmt.Sprint(t.(map[string]any)["tag"]))
		}
		tagStr := ""
		if len(tags) > 0 {
			tagStr = " [" + strings.Join(tags, ", ") + "]"
		}
		fmt.Printf("  %s %s (%s, %s)%s — %s\n", p["avatar"], p["name"], kind, status, tagStr, p["description"])
	}
}

func cmdWhoami(args []string) error {
	return simpleGet(args, "/api/v1/me", func(out map[string]any) {
		printJSON(out)
	})
}

func simpleGet(args []string, path string, human func(map[string]any)) error {
	f := newFlags("get")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}
	out := map[string]any{}
	if err := c.do("GET", path, nil, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	human(out)
	return nil
}

func cmdProfile(args []string) error {
	f := newFlags("profile")
	name := f.fs.String("name", "", "new name")
	avatar := f.fs.String("avatar", "", "new avatar")
	desc := f.fs.String("description", "", "new description")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}

	body := map[string]any{}
	if *name != "" {
		body["name"] = *name
	}
	if *avatar != "" {
		body["avatar"] = *avatar
	}
	if *desc != "" {
		body["description"] = *desc
	}

	out := map[string]any{}
	if len(body) == 0 {
		if err := c.do("GET", "/api/v1/me", nil, &out); err != nil {
			return err
		}
	} else {
		if err := c.do("PATCH", "/api/v1/me", body, &out); err != nil {
			return err
		}
		if *name != "" {
			if p, err := loadProfile(*f.profile); err == nil {
				p.Name = *name
				_ = saveProfile(*f.profile, p)
			}
		}
	}
	printJSON(out)
	return nil
}

func cmdAvatar(args []string) error {
	f := newFlags("avatar")
	remove := f.fs.Bool("remove", false, "revert to the emoji avatar")
	pos := f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}
	if *remove {
		if err := c.do("DELETE", "/api/v1/me/avatar", nil, nil); err != nil {
			return err
		}
		fmt.Println("avatar removed")
		return nil
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: avatar <image-file> | avatar --remove")
	}
	if err := c.setAvatar(pos[0]); err != nil {
		return err
	}
	fmt.Println("avatar updated")
	return nil
}

func cmdOffline(args []string) error {
	f := newFlags("offline")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}
	if err := c.do("POST", "/api/v1/me/offline", nil, nil); err != nil {
		return err
	}
	fmt.Println("marked offline (any new request marks you online again)")
	return nil
}

func cmdParticipants(args []string) error {
	return simpleGet(args, "/api/v1/participants", func(out map[string]any) {
		printParticipants(out["participants"].([]any))
	})
}

func cmdChannels(args []string) error {
	return simpleGet(args, "/api/v1/channels", func(out map[string]any) {
		for _, ch := range out["channels"].([]any) {
			c := ch.(map[string]any)
			marker := ""
			if c["archived"] == true {
				marker = " (archived)"
			}
			fmt.Printf("#%s%s — %s\n", c["name"], marker, c["topic"])
		}
	})
}

func cmdChannelCreate(args []string) error {
	f := newFlags("channel-create")
	topic := f.fs.String("topic", "", "channel topic")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: channel-create <name> [--topic TEXT]")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	out := map[string]any{}
	if err := c.do("POST", "/api/v1/channels", map[string]any{"name": pos[0], "topic": *topic}, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("created #%s\n", out["name"])
	return nil
}

func cmdChannelJoin(args []string) error {
	f := newFlags("channel-join")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: channel-join <channel>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	path := "/api/v1/channels/" + url.PathEscape(strings.TrimPrefix(pos[0], "#")) + "/join"
	if err := c.do("POST", path, nil, nil); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdChannelArchive(args []string, archived bool) error {
	f := newFlags("channel-archive")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: channel-(un)archive <channel>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	path := "/api/v1/channels/" + url.PathEscape(strings.TrimPrefix(pos[0], "#"))
	if err := c.do("PATCH", path, map[string]any{"archived": archived}, nil); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdPost(args []string) error {
	f := newFlags("post")
	thread := f.fs.String("thread", "", "reply in the thread of this message id")
	broadcast := f.fs.Bool("broadcast", false, "mark as a broadcast message")
	var attach multiFlag
	f.fs.Var(&attach, "attach", "file to attach (repeatable)")
	pos := f.parse(args)
	if len(pos) < 2 {
		return fmt.Errorf("usage: post <channel> <text...>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}

	attIDs := []string{}
	for _, path := range attach {
		id, err := c.upload(path)
		if err != nil {
			return fmt.Errorf("attach %s: %w", path, err)
		}
		attIDs = append(attIDs, id)
	}

	body := map[string]any{"body": strings.Join(pos[1:], " "), "broadcast": *broadcast}
	if *thread != "" {
		body["thread_root_id"] = *thread
	}
	if len(attIDs) > 0 {
		body["attachment_ids"] = attIDs
	}

	out := map[string]any{}
	channel := url.PathEscape(strings.TrimPrefix(pos[0], "#"))
	if err := c.do("POST", "/api/v1/channels/"+channel+"/messages", body, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	fmt.Printf("posted %s\n", out["id"])
	return nil
}

func printMessage(m map[string]any, indent string) {
	ts := fmt.Sprint(m["created_at"])
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = t.Local().Format("Jan 02 15:04")
	}
	extra := ""
	if rc, ok := m["reply_count"].(float64); ok && rc > 0 {
		extra = fmt.Sprintf("  [thread: %d replies, id %s]", int(rc), m["id"])
	}
	if m["is_broadcast"] == true {
		extra += "  [broadcast]"
	}
	for _, a := range m["attachments"].([]any) {
		att := a.(map[string]any)
		extra += fmt.Sprintf("  [attachment: %s id %s]", att["filename"], att["id"])
	}
	fmt.Printf("%s%s %s: %s%s\n", indent, ts, m["author_name"], m["body"], extra)
}

func cmdMessages(args []string) error {
	f := newFlags("messages")
	limit := f.fs.Int("limit", 50, "max messages")
	before := f.fs.String("before", "", "only messages before this RFC3339 time")
	beforeID := f.fs.String("before-id", "", "only messages strictly older than this message id (stable pagination cursor)")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: messages <channel>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("limit", fmt.Sprint(*limit))
	if *before != "" {
		q.Set("before", *before)
	}
	if *beforeID != "" {
		q.Set("before_id", *beforeID)
	}
	channel := url.PathEscape(strings.TrimPrefix(pos[0], "#"))
	out := map[string]any{}
	if err := c.do("GET", "/api/v1/channels/"+channel+"/messages?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	for _, m := range out["messages"].([]any) {
		printMessage(m.(map[string]any), "")
	}
	return nil
}

func cmdThread(args []string) error {
	f := newFlags("thread")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: thread <message-id>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	out := map[string]any{}
	if err := c.do("GET", "/api/v1/threads/"+url.PathEscape(pos[0]), nil, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	for i, m := range out["messages"].([]any) {
		indent := ""
		if i > 0 {
			indent = "  ↳ "
		}
		printMessage(m.(map[string]any), indent)
	}
	return nil
}

func cmdUpload(args []string) error {
	f := newFlags("upload")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: upload <file>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	id, err := c.upload(pos[0])
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func cmdDownload(args []string) error {
	f := newFlags("download")
	out := f.fs.String("o", "", "output file (default stdout)")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: download <attachment-id> [-o FILE]")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = c.download(pos[0], os.Stdout)
		return err
	}
	// buffer first so a failed download doesn't truncate an existing file
	var buf bytes.Buffer
	if _, err := c.download(pos[0], &buf); err != nil {
		return err
	}
	return os.WriteFile(*out, buf.Bytes(), 0o644)
}

func cmdTag(args []string, add bool) error {
	f := newFlags("tag")
	pos := f.parse(args)
	if len(pos) < 2 {
		return fmt.Errorf("usage: tag|untag <participant> <tag>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	target := url.PathEscape(strings.TrimPrefix(pos[0], "@"))
	tag := strings.ToLower(strings.TrimSpace(pos[1]))
	if add {
		err = c.do("POST", "/api/v1/participants/"+target+"/tags", map[string]any{"tag": tag}, nil)
	} else {
		err = c.do("DELETE", "/api/v1/participants/"+target+"/tags/"+url.PathEscape(tag), nil, nil)
	}
	if err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdEdit(args []string) error {
	f := newFlags("edit")
	pos := f.parse(args)
	if len(pos) < 2 {
		return fmt.Errorf("usage: edit <message-id> <text...>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	out := map[string]any{}
	body := map[string]any{"body": strings.Join(pos[1:], " ")}
	if err := c.do("PATCH", "/api/v1/messages/"+url.PathEscape(pos[0]), body, &out); err != nil {
		return err
	}
	fmt.Println("edited", out["id"])
	return nil
}

func cmdDelete(args []string) error {
	f := newFlags("delete")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: delete <message-id>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	if err := c.do("DELETE", "/api/v1/messages/"+url.PathEscape(pos[0]), nil, nil); err != nil {
		return err
	}
	fmt.Println("deleted")
	return nil
}

func cmdChannelDelete(args []string) error {
	f := newFlags("channel-delete")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: channel-delete <channel>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	path := "/api/v1/channels/" + url.PathEscape(strings.TrimPrefix(pos[0], "#"))
	if err := c.do("DELETE", path, nil, nil); err != nil {
		return err
	}
	fmt.Println("deleted")
	return nil
}

func cmdSetRole(args []string, role string) error {
	f := newFlags("role")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: promote|demote <participant>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	target := url.PathEscape(strings.TrimPrefix(pos[0], "@"))
	out := map[string]any{}
	if err := c.do("POST", "/api/v1/participants/"+target+"/role", map[string]any{"role": role}, &out); err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", out["name"], out["role"])
	return nil
}

func cmdKick(args []string) error {
	f := newFlags("kick")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: kick <participant>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	target := url.PathEscape(strings.TrimPrefix(pos[0], "@"))
	if err := c.do("DELETE", "/api/v1/participants/"+target, nil, nil); err != nil {
		return err
	}
	fmt.Println("revoked (their messages remain)")
	return nil
}

func cmdLeave(args []string) error {
	f := newFlags("leave")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}
	if err := c.do("DELETE", "/api/v1/participants/me", nil, nil); err != nil {
		return err
	}
	fmt.Println("left the room; this profile's token is now invalid")
	return nil
}

func cmdRoomRename(args []string) error {
	f := newFlags("room-rename")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: room-rename <name...>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	out := map[string]any{}
	if err := c.do("PATCH", "/api/v1/room", map[string]any{"name": strings.Join(pos, " ")}, &out); err != nil {
		return err
	}
	fmt.Println("room renamed to", out["name"])
	return nil
}

func cmdInvites(args []string) error {
	return simpleGet(args, "/api/v1/invites", func(out map[string]any) {
		list, _ := out["invites"].([]any)
		if len(list) == 0 {
			fmt.Println("no invite links")
			return
		}
		for _, it := range list {
			printInvite(it.(map[string]any))
		}
	})
}

func printInvite(v map[string]any) {
	by := "workspace link"
	if n, ok := v["created_by_name"].(string); ok && n != "" {
		by = "by " + n
	}
	extra := ""
	if v["owner_id"] != nil {
		extra += ", binds agents to its owner"
	}
	if e, ok := v["expires_at"].(string); ok && e != "" {
		extra += ", expires " + e
	}
	if u, ok := v["uses"].(float64); ok {
		extra += fmt.Sprintf(", %v uses", u)
	}
	fmt.Printf("%s  %s  (%s%s, %s)\n", v["id"], v["url"], by, extra, v["status"])
}

func cmdInvite(args []string) error {
	f := newFlags("invite")
	expires := f.fs.Duration("expires", 0, "lifetime, e.g. 24h or 168h (0 = never)")
	bind := f.fs.Bool("bind-owner", false, "agents joining with the link become yours (admins; agents always bind)")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}
	body := map[string]any{"expires_in_seconds": int(expires.Seconds()), "bind_owner": *bind}
	out := map[string]any{}
	if err := c.do("POST", "/api/v1/invites", body, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	printInvite(out["invite"].(map[string]any))
	return nil
}

func cmdInviteRevoke(args []string) error {
	f := newFlags("invite-revoke")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: invite-revoke <invite-id>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}
	if err := c.do("DELETE", "/api/v1/invites/"+pos[0], nil, nil); err != nil {
		return err
	}
	fmt.Println("invite link revoked; existing members keep access")
	return nil
}

func cmdMonitor(args []string) error {
	f := newFlags("monitor")
	from := f.fs.Int64("from", -1, "start after this event seq (default: now)")
	once := f.fs.Bool("once", false, "do one long-poll round and exit")
	f.parse(args)
	c, err := f.client()
	if err != nil {
		return err
	}

	cursor := *from
	if cursor < 0 {
		var out struct {
			Cursor int64 `json:"cursor"`
		}
		if err := c.do("GET", "/api/v1/events", nil, &out); err != nil {
			return err
		}
		cursor = out.Cursor
		fmt.Fprintf(os.Stderr, "monitoring from seq %d (JSON lines on stdout)\n", cursor)
	}

	for {
		var out struct {
			Events []json.RawMessage `json:"events"`
			Cursor int64             `json:"cursor"`
		}
		err := c.do("GET", fmt.Sprintf("/api/v1/events?after=%d&wait=25", cursor), nil, &out)
		if err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Status < 500 {
				return err
			}
			fmt.Fprintln(os.Stderr, "monitor: transient error:", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, e := range out.Events {
			fmt.Println(string(e))
		}
		cursor = out.Cursor
		if *once {
			// persist nothing; caller passes --from to continue
			fmt.Fprintf(os.Stderr, "cursor: %d\n", cursor)
			return nil
		}
	}
}

func cmdSearch(args []string) error {
	f := newFlags("search")
	semantic := f.fs.Bool("semantic", false, "use semantic (vector) search")
	channel := f.fs.String("channel", "", "filter: channel name or id")
	author := f.fs.String("author", "", "filter: author name or id")
	since := f.fs.String("since", "", "filter: RFC3339 lower bound")
	until := f.fs.String("until", "", "filter: RFC3339 upper bound")
	thread := f.fs.String("thread", "", "filter: thread root message id")
	hasAtt := f.fs.String("has-attachment", "", "filter: true/false")
	limit := f.fs.Int("limit", 20, "max results")
	pos := f.parse(args)
	if len(pos) < 1 {
		return fmt.Errorf("usage: search <query...>")
	}
	c, err := f.client()
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("q", strings.Join(pos, " "))
	q.Set("limit", fmt.Sprint(*limit))
	for k, v := range map[string]string{
		"channel": *channel, "author": *author, "since": *since,
		"until": *until, "thread": *thread, "has_attachment": *hasAtt,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	path := "/api/v1/search"
	if *semantic {
		path += "/semantic"
	}
	out := map[string]any{}
	if err := c.do("GET", path+"?"+q.Encode(), nil, &out); err != nil {
		return err
	}
	if *f.rawJSON {
		printJSON(out)
		return nil
	}
	results := out["results"].([]any)
	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}
	for _, m := range results {
		mm := m.(map[string]any)
		fmt.Printf("(%.3f) ", mm["score"])
		printMessage(mm, "")
	}
	return nil
}
