module github.com/sylr/fix2/contribs/fixpecho

go 1.27

require github.com/sylr/fix2 v0.0.0

// fix2 is developed alongside: this module builds against the copy in the
// repository.
replace github.com/sylr/fix2 => ../..
