pkg ./internal/api/
run TestHistoryDefault_CategoryFilterNames

[the category name is ignored]
file internal/api/history.go
--- anchor
	catFilter := cmp.Or(formValue(r, "cat"), formValue(r, "category"))
--- replace
	catFilter := cmp.Or(formValue(r, "cat"), "")
--- end

[category outranks cat]
file internal/api/history.go
--- anchor
	catFilter := cmp.Or(formValue(r, "cat"), formValue(r, "category"))
--- replace
	catFilter := cmp.Or(formValue(r, "category"), formValue(r, "cat"))
--- end
