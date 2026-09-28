package contract

import (
	"fmt"
	"strings"
)

// fmtConfiguration / fmtServer / etc. implement String() without pulling in
// extra dependencies. They are deliberately conservative: stable, sorted by
// the producer, and never panic on nil.

func fmtConfiguration(c *Configuration) string {
	var b strings.Builder
	b.WriteString("Configuration{")
	if len(c.Maps) > 0 {
		fmt.Fprintf(&b, "maps=[%d]", len(c.Maps))
		for i, m := range c.Maps {
			fmt.Fprintf(&b, "; maps[%d]=map %s $%s {%d entries}", i, m.Source, m.Name, len(m.Entries))
		}
	}
	if len(c.Servers) > 0 {
		fmt.Fprintf(&b, "servers=[%d]", len(c.Servers))
		for i, s := range c.Servers {
			fmt.Fprintf(&b, "; servers[%d]=%s", i, s.String())
		}
	}
	if len(c.Upstreams) > 0 {
		fmt.Fprintf(&b, " upstreams=[%d]", len(c.Upstreams))
		for i, u := range c.Upstreams {
			fmt.Fprintf(&b, "; upstreams[%d]=%s", i, u.String())
		}
	}
	b.WriteString("}")
	return b.String()
}

func fmtServer(s *Server) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Server{hostname=%q", s.Hostname)
	if len(s.Listens) > 0 {
		fmt.Fprintf(&b, " listens=[")
		for i, l := range s.Listens {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(l.String())
		}
		b.WriteString("]")
	}
	if s.TLSCert != "" {
		fmt.Fprintf(&b, " tlsCert=%q", s.TLSCert)
	}
	if len(s.Locations) > 0 {
		fmt.Fprintf(&b, " locations=[")
		for i, loc := range s.Locations {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(loc.String())
		}
		b.WriteString("]")
	}
	b.WriteString("}")
	return b.String()
}

func fmtListen(l Listen) string {
	var addr string
	if l.Address != "" {
		addr = l.Address
	} else {
		addr = "*"
	}
	mods := ""
	if l.SSL {
		mods += " ssl"
	}
	if l.HTTP2 {
		mods += " http2"
	}
	return fmt.Sprintf("listen=%s:%d%s", addr, l.Port, mods)
}

func fmtLocation(loc *Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Location{path=%q", loc.Path)
	if loc.Upstream != "" {
		fmt.Fprintf(&b, " upstream=%q", loc.Upstream)
	}
	if loc.Rewrite != "" {
		fmt.Fprintf(&b, " rewrite=%q", loc.Rewrite)
	}
	if loc.Redirect != nil {
		fmt.Fprintf(&b, " redirect=%d %q", loc.Redirect.Code, loc.Redirect.URL)
	}
	if loc.RedirectIf != nil {
		fmt.Fprintf(&b, " redirectIf=(~%q → %d %q)", loc.RedirectIf.Match, loc.RedirectIf.Code, loc.RedirectIf.URL)
	}
	if loc.ProxyHost != "" {
		fmt.Fprintf(&b, " proxyHost=%q", loc.ProxyHost)
	}
	if loc.Internal {
		b.WriteString(" internal")
	}
	if loc.MirrorGate != "" {
		fmt.Fprintf(&b, " mirrorGate=%q", loc.MirrorGate)
	}
	if loc.ProxyPassURI != "" {
		fmt.Fprintf(&b, " proxyPassURI=%q", loc.ProxyPassURI)
	}
	if len(loc.Mirrors) > 0 {
		paths := make([]string, 0, len(loc.Mirrors))
		for _, m := range loc.Mirrors {
			paths = append(paths, m.Path)
		}
		fmt.Fprintf(&b, " mirrors=%v", paths)
	}
	if len(loc.RequestHeaders) > 0 {
		fmt.Fprintf(&b, " requestHeaders=[%d]", len(loc.RequestHeaders))
	}
	if len(loc.HideHeaders) > 0 {
		fmt.Fprintf(&b, " hideHeaders=[%d]", len(loc.HideHeaders))
	}
	if len(loc.ResponseHeaders) > 0 {
		fmt.Fprintf(&b, " responseHeaders=[%d]", len(loc.ResponseHeaders))
	}
	if loc.Timeouts != nil {
		if loc.Timeouts.Connect != "" {
			fmt.Fprintf(&b, " connect=%q", loc.Timeouts.Connect)
		}
		if loc.Timeouts.Send != "" {
			fmt.Fprintf(&b, " send=%q", loc.Timeouts.Send)
		}
		if loc.Timeouts.Read != "" {
			fmt.Fprintf(&b, " read=%q", loc.Timeouts.Read)
		}
	}
	b.WriteString("}")
	return b.String()
}

func fmtUpstream(u *Upstream) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Upstream{name=%q", u.Name)
	if len(u.Endpoints) > 0 {
		fmt.Fprintf(&b, " endpoints=[")
		for i, e := range u.Endpoints {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(e.String())
		}
		b.WriteString("]")
	}
	b.WriteString("}")
	return b.String()
}

func fmtEndpoint(e Endpoint) string {
	if e.Socket != "" {
		return fmt.Sprintf("unix:%s(%s)", e.Socket, e.IP)
	}
	state := "up"
	if !e.Ready {
		state = "down"
	}
	return fmt.Sprintf("%s:%d(%s)", e.IP, e.Port, state)
}
