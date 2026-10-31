// uws session / webtouch session —— 对外 session 生命周期。
//
// CLI 只携带 session_id。状态在本机 lease（pkg/clisession），不在本进程里。
// 契约：docs/cli-session-contract-2026-10-31.md。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"uws/pkg/clisession"
	"uws/pkg/fingerprint"
)

var errSessionHelp = errors.New("session help")

type sessionOpts struct {
	id      string
	channel string
	account string
	profile string
	site    string
	attach  string
	engine  string
	all     bool
	text    bool
	rest    []string
}

func runSession(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stdout, sessionGuide())
		return nil
	}
	cmd := args[0]
	opts, err := parseSessionArgs(args[1:])
	if errors.Is(err, errSessionHelp) {
		fmt.Fprint(os.Stdout, sessionGuide())
		return nil
	}
	if err != nil {
		return emitSessionError(err)
	}
	switch cmd {
	case "create", "list", "ls", "show", "use", "close", "rm", "delete":
	default:
		return emitSessionError(fmt.Errorf("未知子命令 session %s（支持 create | use | show | list | close）", cmd))
	}
	store, err := clisession.Open(clisession.DefaultPath())
	if err != nil {
		return emitSessionError(err)
	}
	switch cmd {
	case "create":
		return sessionCreate(store, opts)
	case "list", "ls":
		return sessionList(store, opts)
	case "show":
		id, err := sessionID(opts)
		if err != nil {
			return emitSessionError(err)
		}
		view, ok, err := store.Lookup(id)
		if err != nil {
			return emitSessionError(err)
		}
		if !ok {
			return emitSessionError(&clisession.Error{Code: clisession.CodeNotFound, Message: "session 不存在"})
		}
		return printSession(view, opts.text)
	case "use":
		id, err := sessionID(opts)
		if err != nil {
			return emitSessionError(err)
		}
		in := clisession.UseInput{}
		if opts.site != "" {
			in.SiteConversationID = &opts.site
		}
		view, err := store.Use(id, in)
		if err != nil {
			return emitSessionError(err)
		}
		return printSession(view, opts.text)
	case "close", "rm", "delete":
		id, err := sessionID(opts)
		if err != nil {
			return emitSessionError(err)
		}
		view, err := store.Close(id)
		if err != nil {
			return emitSessionError(err)
		}
		return printSession(view, opts.text)
	default:
		return emitSessionError(fmt.Errorf("未知子命令 session %s", cmd))
	}
}

func sessionCreate(store *clisession.Store, opts sessionOpts) error {
	if opts.account != "" || opts.profile != "" {
		reg, err := fingerprint.OpenRegistry("")
		if err != nil {
			return emitSessionError(&clisession.Error{Code: clisession.CodeProfileUnbound, Message: "打不开身份注册表，拒绝创建会话"})
		}
		if opts.account != "" {
			store.SetResolver(registryResolver{reg})
		} else if _, err := reg.Load(opts.profile); err != nil {
			return emitSessionError(&clisession.Error{Code: clisession.CodeProfileUnbound, Message: "Profile 不存在，拒绝创建会话"})
		}
	}
	view, err := store.Create(clisession.CreateInput{
		ID:                 opts.id,
		Channel:            opts.channel,
		Account:            opts.account,
		ProfileID:          opts.profile,
		SiteConversationID: opts.site,
		Attach:             opts.attach,
		EngineAddr:         opts.engine,
	})
	if err != nil {
		return emitSessionError(err)
	}
	return printSession(view, opts.text)
}

func sessionList(store *clisession.Store, opts sessionOpts) error {
	views, err := store.List(opts.all)
	if err != nil {
		return emitSessionError(err)
	}
	if opts.text {
		if len(views) == 0 {
			fmt.Println("没有 session")
			return nil
		}
		for _, v := range views {
			fmt.Printf("%s\t%s\t%s\t%s\n", v.SessionID, v.State, v.Channel, v.Runtime)
		}
		return nil
	}
	return writeSessionJSON(clisession.ListEnvelope{OK: true, Contract: clisession.ContractV1, Sessions: views})
}

func sessionID(opts sessionOpts) (string, error) {
	if opts.id != "" {
		return opts.id, nil
	}
	if len(opts.rest) == 1 {
		return opts.rest[0], nil
	}
	if len(opts.rest) > 1 {
		return "", &clisession.Error{Code: clisession.CodeInvalid, Message: "只能指定一个 session_id"}
	}
	if v := strings.TrimSpace(os.Getenv("WEBTOUCH_SESSION")); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("UWS_SESSION")); v != "" {
		return v, nil
	}
	return "", &clisession.Error{Code: clisession.CodeInvalid, Message: "缺少 session_id（--session、位置参数，或 WEBTOUCH_SESSION / UWS_SESSION）"}
}

func parseSessionArgs(args []string) (sessionOpts, error) {
	var opts sessionOpts
	for i := 0; i < len(args); i++ {
		a := args[i]
		take := func() (string, error) {
			if i+1 >= len(args) {
				return "", &clisession.Error{Code: clisession.CodeInvalid, Message: "参数 " + a + " 缺少取值"}
			}
			i++
			return args[i], nil
		}
		switch a {
		case "--json":
			opts.text = false
		case "--text":
			opts.text = true
		case "--all":
			opts.all = true
		case "--session", "--id":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.id = v
		case "--channel":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.channel = v
		case "--account":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.account = v
		case "--profile":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.profile = v
		case "--site-conversation":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.site = v
		case "--attach":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.attach = v
		case "--engine-addr":
			v, err := take()
			if err != nil {
				return opts, err
			}
			opts.engine = v
		case "-h", "--help":
			return opts, errSessionHelp
		default:
			if strings.HasPrefix(a, "-") {
				return opts, &clisession.Error{Code: clisession.CodeInvalid, Message: "未知参数 " + a}
			}
			opts.rest = append(opts.rest, a)
		}
	}
	return opts, nil
}

type registryResolver struct {
	reg *fingerprint.Registry
}

func (r registryResolver) Resolve(account string) (string, error) {
	profile, err := r.reg.Resolve(account)
	if err != nil {
		return "", err
	}
	return profile.ProfileID, nil
}

func printSession(view clisession.View, text bool) error {
	if text {
		fmt.Printf("%s\t%s\t%s\t%s\n", view.SessionID, view.State, view.Channel, view.Runtime)
		return nil
	}
	return writeSessionJSON(clisession.Envelope{OK: true, Contract: clisession.ContractV1, View: view})
}

func emitSessionError(err error) error {
	code := "internal_error"
	msg := err.Error()
	var ce *clisession.Error
	if errors.As(err, &ce) {
		code = ce.Code
		msg = ce.Message
	}
	env := clisession.ErrorEnvelope{OK: false, Contract: clisession.ContractV1}
	env.Error.Code = code
	env.Error.Message = msg
	_ = writeSessionJSON(env)
	return err
}

func writeSessionJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func sessionGuide() string {
	return `webtouch session —— 本机有状态 session（对外契约是 CLI）

用法:
  webtouch session create [--channel <id>] [--account <账号>] [--profile <id>]
                          [--site-conversation <id>] [--attach <cdp>] [--engine-addr <addr>]
  webtouch session use --session <id> [--site-conversation <id>]
  webtouch session show --session <id>
  webtouch session list [--all]
  webtouch session close --session <id>

句柄也可写在位置参数，或环境变量 WEBTOUCH_SESSION / UWS_SESSION（create 除外）。
stdout 默认是 JSON（webtouch.cli.session.v1）。--text 打一行人类摘要。
uws 是 webtouch 的兼容别名。

Hub 只应记账 JSON 里的 hub.channel 与 hub.site_conversation_id。
account / profile 留在本机。凭据与 @vault: 不得出现在参数里。
create 不弹出窗口，也不为同一账号新建指纹；账号必须已经 bind。
`
}
