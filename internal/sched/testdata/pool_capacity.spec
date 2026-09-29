pkg ./internal/sched/
run ^TestPools_SetCapacityFloorsAtOne$

[leasePool.setCapacity takes a capacity of zero]
file internal/sched/pool.go
--- anchor
func (p *leasePool) setCapacity(c int) {
	if c < 1 {
--- replace
func (p *leasePool) setCapacity(c int) {
	if c < 0 {
--- end

[slotPool.setCapacity takes a capacity of zero]
file internal/sched/pool.go
--- anchor
func (p *slotPool) setCapacity(c int) {
	if c < 1 {
--- replace
func (p *slotPool) setCapacity(c int) {
	if c < 0 {
--- end
