package backend

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coyove/enn"
	"github.com/coyove/enn/server/common"
)

func (tb *Backend) Post(article *enn.Article) error {
	// log.Printf("post: %#v", article.Header)

	// Check special subject 'd' issued by mods, which can delete the refered article
	subject := article.Header.Get("Subject")
	switch strings.TrimSpace(subject) {
	case "d":
		if tb.AuthObject == nil {
			return enn.ErrNotAuthenticated
		}
		if !ImplIsMod(tb) {
			return enn.ErrNotMod
		}
		refer := article.Header.Get("References")
		if refer == "" {
			return &enn.NNTPError{Code: 441, Msg: "Please refer an article"}
		}
		common.L("delete article %q, auth: %#v", refer, tb.AuthObject)
		return tb.DeleteArticle(common.ExtractMsgID(refer))
	}

	// Check subject length
	if utf8.RuneCountInString(subject) > 128 {
		idx := strings.Index(subject, "=?")
		idxend := strings.Index(subject, "?=")
		if idx == -1 || idxend == -1 || idxend <= idx {
			rs := []rune(subject)
			article.Header.Set("Subject", string(rs[:64])+string(rs[len(rs)-64:]))
		} else {
			article.Header.Set("Subject", subject[idx:idxend+2])
		}
	}

	// Check sender address, no mod spoof
	email := common.ExtractEmail(article.Header.Get("From"))
	isMod := false
	if tb.Mods[email] != nil {
		if tb.AuthObject == nil {
			return enn.ErrNotAuthenticated
		}
		if !ImplIsMod(tb) {
			return enn.ErrNotMod
		}
		isMod = true
	}

	// Check IP throt
	if !isMod {
		ip := article.RemoteAddr.String()
		v, ok := tb.ipCache.Get(ip)
		if ok {
			if diff := time.Since(v.(time.Time)); diff < tb.PostInterval {
				return &enn.NNTPError{Code: 441, Msg: fmt.Sprintf("Post cooldown (wait %v)", tb.PostInterval-diff)}
			}
		}
		tb.ipCache.Add(ip, time.Now())
	}

	// Read the body, check global max posting size limitation
	mps := ImplMaxPostSize(tb)
	buf := &bytes.Buffer{}
	n, err := io.Copy(buf, io.LimitReader(article.Body, mps))
	if err != nil {
		return err
	}
	if n >= mps {
		return &enn.NNTPError{Code: 441, Msg: fmt.Sprintf("Post too large (max %s)", common.FormatSize(mps))}
	}

	// If client sent a custom Message-Id, we will try to use it
	var msgID string
	if msgid := article.Header["Message-Id"]; len(msgid) > 0 {
		msgID = common.ExtractMsgID(msgid[0])
		common.L("post: predefined msgid:", msgID)
		delete(article.Header, "Message-Id")
	} else {
		msgID = strconv.FormatInt(time.Now().Unix(), 36) + strconv.FormatUint(uint64(rand.Uint32()), 36)
	}

	// Fill in custom headers
	article.Header["X-Message-Id"] = []string{msgID}
	article.Header["X-Remote-Ip"] = []string{fmt.Sprint(article.RemoteAddr)}
	article.Header["X-Lines"] = []string{fmt.Sprint(bytes.Count(buf.Bytes(), []byte("\n")))}
	article.Header["X-Length"] = []string{fmt.Sprint(buf.Len())}

	a := common.Article{
		Headers: article.Header,
		Body:    buf.Bytes(),
		Refer:   article.Header["Newsgroups"],
	}

	// Fill in article referers, note two forms:
	//   1. []string{"A", "B", ...}
	//   2. []string{"A,B,..."}
	for i := len(a.Refer) - 1; i >= 0; i-- {
		ar := a.Refer[i]
		groups := strings.Split(ar, ",")
		if len(groups) > 1 {
			for i := range groups {
				groups[i] = strings.TrimSpace(groups[i])
			}
			a.Refer = append(a.Refer[:i], append(groups, a.Refer[i+1:]...)...)
		}
	}

	// If Message-Id has been used, then return error
	if _, ok := tb.Articles[common.MsgIDToRawMsgID(msgID, nil)]; ok {
		return enn.ErrPostingFailed
	}

	// Write header+body to disk
	ar, err := tb.writeData(a.Marshal())
	if err != nil {
		return err
	}
	ar.RawMsgID = common.MsgIDToRawMsgID(msgID, nil)

	// Write index to disk and then append it to each newsgroup
	var postSuccess int
	var lastError = enn.ErrPostingFailed
	for _, g := range a.Refer {
		g, ok := tb.Groups[g]
		if !ok {
			continue
		}

		if g.Group.Posting == enn.PostingNotPermitted {
			if !ImplIsMod(tb) {
				continue
			}
		}

		if limit := g.BaseInfo.MaxPostSize * 4 / 3; g.BaseInfo.MaxPostSize != 0 && n > limit {
			common.L("post: %q large article %v (%d <-> %d)", g.Group.Name, msgID, n, limit)
			return &enn.NNTPError{Code: 441, Msg: fmt.Sprintf("Post too large (max %s)", common.FormatSize(limit))}
			continue
		}

		if err := tb.writeIndex([]byte(fmt.Sprintf("\nA%s %s %d %s %s",
			g.Group.Name,
			msgID,
			ar.Index,
			strconv.FormatInt(ar.Offset, 36),
			strconv.FormatInt(ar.Length, 36)))); err != nil {
			common.L("post: %q write index %err", g.Group.Name, err)
			continue
		}

		g.Append(tb, ar)
		g.Group.Low = int64(g.Articles.Low() + 1)
		g.Group.High = int64(g.Articles.High()+1) - 1
		g.Group.Count = int64(g.Articles.Len())

		common.L("post: %q new article %v", g.Group.Name, msgID)
		postSuccess++
	}

	if postSuccess > 0 {
		tb.Articles[common.MsgIDToRawMsgID(msgID, nil)] = ar
	} else {
		return lastError
	}
	return nil
}
