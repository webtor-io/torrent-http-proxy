package services

import (
	"fmt"
	"html"
	"net/http"
)

// errorPage is what a refusal tells a person: what happened and what to do.
// Texts are constants or carry only integers from the config: a page never
// holds anything from the request, since it renders on the content host.
// No brand or plan wording: the self-hosted image serves these too.
type errorPage struct{ title, text string }

var (
	pageLinkInvalid     = errorPage{"This link has expired or is not valid", "Links work for a limited time. Open the file again from its page to get a new link."}
	pageLinkBroken      = errorPage{"This link is broken", "Open the file again from its page."}
	pagePrivate         = errorPage{"This torrent is private", "Only the account that added it can open it. Sign in with that account and open the file from the torrent's page."}
	pageNotAvailable    = errorPage{"This torrent is not available", "It cannot be opened."}
	pageTorrentNotFound = errorPage{"This torrent was not found", "Open the file again from its page."}
	pageTemporary       = errorPage{"Temporarily unavailable", "Try again in a minute."}
	pageNotFound        = errorPage{"File not found", "Open the file again from its page."}
	pageOtherNetwork    = errorPage{"This link was made for another network", "Open the file again from its page."}
	pageServerError     = errorPage{"Something went wrong on our side", "Try again in a minute."}
	pageTooMany         = errorPage{"Too many requests", "Try again in a minute."}
)

// errorPageHTML is under 2048 bytes with any text here, so net/http sets
// Content-Length and a HEAD gets the GET's headers. color-scheme lets the
// browser follow the reader's light or dark preference.
const errorPageHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>%[1]s</title>
<style>body{font:17px/1.5 system-ui,sans-serif;max-width:34em;margin:15vh auto;padding:0 16px}h1{font-size:1.4em;line-height:1.3}</style>
</head><body><h1>%[1]s</h1><p>%[2]s</p></body></html>
`

// isNavigation: the request is a browser's top-level navigation, the one
// case where a body is shown to a person. Browsers send it on navigation
// only (Safari since 16.4); download managers, apps, players, the archiver
// and FFmpeg do not; nginx-vod passes the viewer's headers on with its
// reads. Accept is no test: JDownloader and yt-dlp send text/html.
func isNavigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Dest") == "document"
}

// writeRefusal answers status, with p as a page to a navigation and bare to
// everything else (curl without -f writes a body into the file). Headers the
// caller set (Retry-After, CORS) stay.
func writeRefusal(w http.ResponseWriter, r *http.Request, status int, p errorPage) {
	if !isNavigation(r) {
		w.WriteHeader(status)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, errorPageHTML, html.EscapeString(p.title), html.EscapeString(p.text))
}

// page is the private gate's refusal page: a failed lookup by its status.
func (v gateVerdict) page() errorPage {
	switch {
	case v.outcome == gateUnbound, v.outcome == gateNoUid:
		return pagePrivate
	case v.outcome == gateMalformedHash:
		return pageLinkInvalid
	case v.status == http.StatusForbidden:
		return pageNotAvailable
	case v.status == http.StatusNotFound:
		return pageTorrentNotFound
	}
	return pageTemporary
}

// refusalPage is the page for a refusal by the cap reason (Acquire's).
func (l *SessionLimiter) refusalPage(reason string) errorPage {
	switch reason {
	case "path":
		return errorPage{"Too many connections to this file", fmt.Sprintf("Your session already has %d requests open to this file, usually a download manager splitting it into parts. Let them finish or set the download manager to %d connections or fewer, then try again.", l.maxPerPath, l.maxPerPath)}
	case "total":
		return errorPage{"Too many downloads at once", fmt.Sprintf("Your session already has %d downloads and streams open. Wait for some to finish, then try again.", l.maxTotal)}
	case "bigfiles":
		return errorPage{"Too many files from this torrent at once", fmt.Sprintf("Up to %d large files from one torrent can download at the same time. Wait for one to finish, then try again.", l.maxBigFilesPerHash)}
	case "ips":
		return errorPage{"This file was opened from too many networks", fmt.Sprintf("In the last minute this file was requested from more than %d networks in one session, which usually means the link was shared. Open the file from its page yourself and try again in a minute.", l.maxIPsPerSession)}
	}
	return pageTooMany
}
