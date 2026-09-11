pkg ./scripts/check_doc_citations/
run TestSectionRE_RecognisesTheFormsTheTreeUses|TestSectionResolves|TestHeadingWords

[the section check always resolves, so the gate is inert]
file scripts/check_doc_citations/main.go
--- anchor
	want := headingWords(name)
	if len(want) == 0 {
		return true
	}
--- replace
	want := headingWords(name)
	if true {
		return true
	}
--- end

[the two-word comparison degenerates to one word, so a wrong section passes]
file scripts/check_doc_citations/main.go
--- anchor
		n := min(len(want), len(h.words), 2)
--- replace
		n := min(len(want), len(h.words), 1)
--- end

[a numeric citation matches any numbered heading, not the one it names]
file scripts/check_doc_citations/main.go
--- anchor
			if h.num == num {
--- replace
			if h.num != "" {
--- end

[a numbered list item is not treated as a section anchor]
file scripts/check_doc_citations/main.go
--- anchor
		if m := listItemRE.FindStringSubmatch(line); m != nil {
--- replace
		if m := listItemRE.FindStringSubmatch(line); false {
--- end

[the leading section number is not stripped, so numbered headings never match]
file scripts/check_doc_citations/main.go
--- anchor
			s = s[i+1:]
--- replace
			s = s
--- end
