pkg ./internal/config/
run TestValidate_FieldsConsumersUsedToRedefault|TestSet_AdminDirExpandingToEmptyIsRefused

[admin_dir left unvalidated]
file internal/config/validate.go
--- anchor
	if strings.TrimSpace(g.AdminDir) == "" {
--- replace
	if false && strings.TrimSpace(g.AdminDir) == "" {
--- end

[max_compute_slots left unvalidated]
file internal/config/validate.go
--- anchor
	if err := positive("max_compute_slots", d.MaxComputeSlots); err != nil {
--- replace
	if err := positive("max_compute_slots", d.MaxComputeSlots); false && err != nil {
--- end

[direct_unpack_threads left unvalidated]
file internal/config/validate.go
--- anchor
	if err := nonNegative("direct_unpack_threads", p.DirectUnpackThreads); err != nil {
--- replace
	if err := nonNegative("direct_unpack_threads", p.DirectUnpackThreads); false && err != nil {
--- end

[category priority left unvalidated]
file internal/config/validate.go
--- anchor
	if _, ok := categoryPriority(c.Priority); !ok && c.Priority != int(constants.DefaultPriority) {
--- replace
	if _, ok := categoryPriority(c.Priority); false && !ok && c.Priority != int(constants.DefaultPriority) {
--- end

[Default (inherit) refused as a category priority]
file internal/config/validate.go
--- anchor
	if _, ok := categoryPriority(c.Priority); !ok && c.Priority != int(constants.DefaultPriority) {
--- replace
	if _, ok := categoryPriority(c.Priority); !ok && c.Priority != int(constants.DefaultPriority)+1 {
--- end
