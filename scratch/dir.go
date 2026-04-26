package main
import (
    "fmt"
    "path/filepath"
)
func main() {
    fmt.Printf("%q\n", filepath.Dir(""))
    fmt.Printf("%q\n", filepath.Dir("file.txt"))
}
