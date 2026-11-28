package panel

import (
	"sort"
	"strconv"
	"strings"
)

// The panel speaks Japanese and English, chosen by the browser's
// Accept-Language (ADR 0031). Every text the pages show is here, so a
// key missing from one language is a test failure, not a blank.

type lang string

const (
	langEnglish  lang = "en"
	langJapanese lang = "ja"
)

// messages are the texts of the pages by language and key.
var messages = map[lang]map[string]string{
	langEnglish: {
		"title":              "nakama control panel",
		"signOut":            "Sign out",
		"version":            "Version",
		"homeLead":           "This is the nakama control panel. Signing in to AWS and managing instances are being added to it step by step.",
		"sessionNote":        "You are signed out after 30 minutes without use, and 12 hours after signing in at the latest.",
		"reopen":             "Run %s on this machine to open it again.",
		"linkUsedTitle":      "This sign-in link cannot be used",
		"linkUsedBody":       "It has expired or was already used.",
		"sessionEndedTitle":  "You are not signed in",
		"sessionEndedBody":   "Your control panel session has ended, or this browser was not signed in.",
		"signedOutTitle":     "Signed out",
		"signedOutBody":      "You signed out of the control panel.",
		"notPanelTitle":      "This is not the nakama control panel",
		"notPanelBody":       "The control panel answers only at its own address.",
		"otherUserTitle":     "This control panel belongs to another user",
		"otherUserBody":      "The control panel only answers the OS user who runs it.",
		"refusedTitle":       "The request was refused",
		"refusedBody":        "The control panel only accepts requests from its own pages.",
		"notFoundTitle":      "Not found",
		"notFoundBody":       "The control panel has no such page.",
		"methodTitle":        "Method not allowed",
		"methodBody":         "This page does not accept that kind of request.",
		"internalErrorTitle": "Something went wrong",
		"internalErrorBody":  "The control panel could not finish the request; its window shows the details.",
	},
	langJapanese: {
		"title":              "nakama 管理画面",
		"signOut":            "ログアウト",
		"version":            "バージョン",
		"homeLead":           "nakama の管理画面です。AWS へのログインやインスタンスの操作は、これから順に追加します。",
		"sessionNote":        "30 分操作しないとログアウトします。ログインから 12 時間たったときもログアウトします。",
		"reopen":             "この端末で %s を実行すると、開き直せます。",
		"linkUsedTitle":      "このリンクではログインできません",
		"linkUsedBody":       "有効期限が切れたか、すでに使われました。",
		"sessionEndedTitle":  "ログインしていません",
		"sessionEndedBody":   "管理画面のセッションが終わったか、このブラウザーではログインしていません。",
		"signedOutTitle":     "ログアウトしました",
		"signedOutBody":      "管理画面からログアウトしました。",
		"notPanelTitle":      "nakama の管理画面のアドレスではありません",
		"notPanelBody":       "管理画面は自分のアドレスでだけ応答します。",
		"otherUserTitle":     "ほかのユーザーの管理画面です",
		"otherUserBody":      "管理画面は、それを起動した OS のユーザーにだけ応答します。",
		"refusedTitle":       "この要求は受け付けられません",
		"refusedBody":        "管理画面は、自分のページからの要求だけを受け付けます。",
		"notFoundTitle":      "ページが見つかりません",
		"notFoundBody":       "管理画面にそのページはありません。",
		"methodTitle":        "この方法では要求できません",
		"methodBody":         "このページはその種類の要求を受け付けません。",
		"internalErrorTitle": "エラーが起きました",
		"internalErrorBody":  "管理画面は要求を処理できませんでした。詳しくは管理画面のウィンドウに表示しています。",
	},
}

// pickLang returns the first of Japanese and English in the browser's
// order of preference (q values), English when it names neither.
func pickLang(acceptLanguage string) lang {
	type pref struct {
		l     lang
		q     float64
		order int
	}
	var prefs []pref
	for i, part := range strings.Split(acceptLanguage, ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		tag := strings.ToLower(strings.TrimSpace(fields[0]))
		q := 1.0
		for _, f := range fields[1:] {
			f = strings.TrimSpace(f)
			if v, ok := strings.CutPrefix(f, "q="); ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					q = n
				}
			}
		}
		primary, _, _ := strings.Cut(tag, "-")
		if q <= 0 {
			continue
		}
		switch primary {
		case string(langJapanese):
			prefs = append(prefs, pref{langJapanese, q, i})
		case string(langEnglish):
			prefs = append(prefs, pref{langEnglish, q, i})
		}
	}
	if len(prefs) == 0 {
		return langEnglish
	}
	sort.SliceStable(prefs, func(a, b int) bool { return prefs[a].q > prefs[b].q })
	return prefs[0].l
}
