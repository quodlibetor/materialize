// Copyright 2015 - 2019 The Cockroach Authors. All rights reserved.
// Copyright 2019 Materialize, Inc. All rights reserved.
//
// This file is part of Materialize. Materialize may not be used or
// distributed without the express permission of Materialize, Inc.
//
// This file is derived from the docgen tool in CockroachDB. The
// original files were retrieved on Nov 12, 2019 from:
//
//     https://github.com/cockroachdb/cockroach/tree/d2f7fbf5dd1fc1a099bbad790a2e1f7c60a66cc3/pkg/cmd/docgen
//
// The original source code is subject to the terms of the Apache
// 2.0 license, a copy of which can be found in the LICENSE file at the
// root of this repository.

package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/yosssi/gohtml"
	"golang.org/x/net/html"
	"golang.org/x/sys/unix"
)

const (
	rrAddr      = "http://bottlecaps.de/rr/ui"
	rrWatermark = "<!-- generated by Railroad Diagram Generator https://www.bottlecaps.de/rr/ui -->\n"
	bnfMD5Path  = "./bnfmd5.json"
)

var bnfMD5s map[string]string

func getMD5Hash(text []byte) string {
	hasher := md5.New()
	hasher.Write(text)
	return hex.EncodeToString(hasher.Sum(nil))
}

func getOldBNFMD5s() {

	oldBNFMD5s, err := ioutil.ReadFile(bnfMD5Path)

	if err != nil {
		log.Fatalf("Cannot read %s: %s\n", bnfMD5Path, err)
	}

	bnfMD5s = make(map[string]string)

	err = json.Unmarshal(oldBNFMD5s, &bnfMD5s)

	if err != nil {
		log.Fatalf("Cannot unmarshal %s into bnfMD5s\n", bnfMD5Path, err)
	}

}

// ebnfToXHTML generates the railroad XHTML from a EBNF file.
func ebnfToXHTML(bnf []byte) ([]byte, error) {

	v := url.Values{}
	v.Add("frame", "diagram")
	v.Add("text", string(bnf))
	v.Add("width", "620")
	v.Add("options", "eliminaterecursion")
	v.Add("options", "factoring")
	v.Add("options", "inline")

	resp, err := http.Post(rrAddr, "application/x-www-form-urlencoded", strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	return body, nil
}

// xhtmlToHTML converts the XHTML railroad diagrams to HTML.
func xhtmlToHTML(xhtml []byte) (string, error) {
	r := bytes.NewReader(xhtml)
	b := new(bytes.Buffer)
	z := html.NewTokenizer(r)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			err := z.Err()
			if err == io.EOF {
				break
			}
			return "", z.Err()
		}
		t := z.Token()
		switch t.Type {
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			idx := strings.IndexByte(t.Data, ':')
			t.Data = t.Data[idx+1:]
		}
		var na []html.Attribute
		for _, a := range t.Attr {
			if strings.HasPrefix(a.Key, "xmlns") {
				continue
			}
			na = append(na, a)
		}
		t.Attr = na
		b.WriteString(t.String())
	}

	doc, err := goquery.NewDocumentFromReader(b)
	if err != nil {
		return "", err
	}
	defs := doc.Find("defs")
	dhtml, err := defs.First().Html()
	if err != nil {
		return "", err
	}
	doc.Find("head").AppendHtml(dhtml)
	defs.Remove()
	doc.Find("svg").First().Remove()
	doc.Find("meta[http-equiv]").Remove()
	doc.Find("head").PrependHtml(`<meta charset="UTF-8">`)
	doc.Find("a[name]:not([href])").Each(func(_ int, s *goquery.Selection) {
		name, exists := s.Attr("name")
		if !exists {
			return
		}
		s.SetAttr("href", "#"+name)
	})
	s, err := doc.Find("html").Html()
	s = "<!DOCTYPE html><html>" + s + "</html>"
	return s, err
}

// extractSVGDiagram extracts the embedded SVG diagram.
func extractSVGDiagram(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}

	// The railroad diagram page we received has two SVGs;
	// we want the first one.
	svgSelection := doc.Find("body svg").Eq(0)

	svgSelection.Children().Each(func(i int, s *goquery.Selection) {
		// Remove all anchor tags, which Materialize
		// doesn't use.
		if s.Is("a") {
			s.ReplaceWithSelection(s.Children())
		}
	})

	return goquery.OuterHtml(svgSelection)
}

func addRRWatermark(s string) string {
	return s + rrWatermark
}

func formatHTML(s string) string {
	defer func(oldCondense bool) {
		gohtml.Condense = oldCondense
	}(gohtml.Condense)
	gohtml.Condense = true
	s = gohtml.Format(s)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// convertBNFtoSVG finds all .bnf files in srcDir,
// converts them to SVG files, and writes those SVG
// files to dstDir as .html files appropriate for
// being included in Hugo.
func convertBNFtoSVG(srcDir string, dstDir string) {
	bnfFilename := regexp.MustCompile(`(.+?)\.bnf$`)
	files, err := ioutil.ReadDir(srcDir)
	if err != nil {
		log.Fatal(err)
	}

	getOldBNFMD5s()

	fmt.Println("Writing updated, converted BNF files to...")

	// Find all BNF files.
	for _, f := range files {

		if bnfFilename.Match([]byte(f.Name())) {

			fp := filepath.Join(srcDir, f.Name())

			bnf, err := ioutil.ReadFile(fp)
			if err != nil {
				log.Fatalf("Cannot read %s: %s\n", f.Name(), err)
			}

			oldMD5, ok := bnfMD5s[fp]
			newMD5 := getMD5Hash(bnf)

			// If we've generated this file before,
			// and the hash of the BNF is the same,
			// do not regenerate.
			if ok {
				if oldMD5 == newMD5 {
					continue
				}
			}

			bnfMD5s[fp] = newMD5

			xhtml, err := ebnfToXHTML(bnf)
			if err != nil {
				panic(fmt.Sprintf("EBNF to XHTML conversion failed for %s: %v\n", f.Name(), err))
			}

			html, err := xhtmlToHTML(xhtml)
			if err != nil {
				panic(fmt.Sprintf("XHTML to HTML conversion failed for %s: %v", f.Name(), err))
			}

			svg, err := extractSVGDiagram(html)
			if err != nil {
				panic(fmt.Sprintf("Extracting SVG from HTML failed for %s: %v", f.Name(), err))
			}

			svg = formatHTML(svg)
			waterMarkedSvg := addRRWatermark(svg)

			dstFilename := bnfFilename.ReplaceAllString(f.Name(), filepath.Join(dstDir, "$1.html"))
			err = ioutil.WriteFile(dstFilename, []byte(waterMarkedSvg), 0644)
			if err != nil {
				panic(fmt.Sprintf("Failed to write %s: %v", dstFilename, err))
			}
			fmt.Println("\t", dstFilename)
		}
	}

	fmt.Println("Updating BNF MD5s at", bnfMD5Path)

	bnfMD5JSON, err := json.MarshalIndent(bnfMD5s, "", "    ")
	bnfMD5JSON = append(bnfMD5JSON, "\n"...)
	err = ioutil.WriteFile(bnfMD5Path, bnfMD5JSON, 0644)
	if err != nil {
		panic(fmt.Sprintf("Failed to write %s: %v", bnfMD5Path, err))
	}
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("USAGE: rr-diagram-gen <srcDir> <dstDir>\n")
	}

	srcDir := os.Args[1]
	dstDir := os.Args[2]

	if _, err := os.Stat(srcDir); os.IsNotExist(err) {
		log.Fatalf("srcDir (%s) does not exist.\n", srcDir)
	}

	if _, err := os.Stat(dstDir); os.IsNotExist(err) {
		log.Fatalf("dstDir (%s) does not exist.\n", srcDir)
	}

	if unix.Access(dstDir, unix.W_OK) != nil {
		log.Fatalf("dstDir (%s) is not writable\n", dstDir)
	}

	convertBNFtoSVG(srcDir, dstDir)
}
