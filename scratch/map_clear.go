package main
import "fmt"
func main() {
    m := make(map[string][]string)
    m["a"] = append(m["a"], "foo")
    fmt.Println(len(m))
    clear(m)
    fmt.Println(len(m))
}
