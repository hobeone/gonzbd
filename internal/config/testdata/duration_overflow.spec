pkg ./internal/config/
run TestValidate_DurationFieldsRefuseOverflow

[the bound never refuses]
file internal/config/validate.go
--- anchor
	if int64(v) > int64(math.MaxInt64/unit) {
--- replace
	if false {
--- end

[the bound off by one, refusing the largest value that fits]
file internal/config/validate.go
--- anchor
	if int64(v) > int64(math.MaxInt64/unit) {
--- replace
	if int64(v) >= int64(math.MaxInt64/unit) {
--- end

[dirscan_speed not bounded]
file internal/config/validate.go
--- anchor
		if err := fitsDuration("dirscan_speed", g.DirscanSpeed, time.Second); err != nil {
--- replace
		if err := error(nil); err != nil {
--- end

[propagation_delay not bounded]
file internal/config/validate.go
--- anchor
	if err := fitsDuration("propagation_delay", d.PropagationDelay, time.Minute); err != nil {
--- replace
	if err := error(nil); err != nil {
--- end

[propagation_delay bounded in seconds, not minutes]
file internal/config/validate.go
--- anchor
	if err := fitsDuration("propagation_delay", d.PropagationDelay, time.Minute); err != nil {
--- replace
	if err := fitsDuration("propagation_delay", d.PropagationDelay, time.Second); err != nil {
--- end

[server timeout not bounded]
file internal/config/validate.go
--- anchor
	if err := positive("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := fitsDuration("timeout", s.Timeout, time.Second); err != nil {
--- replace
	if err := positive("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := error(nil); err != nil {
--- end

[script timeout not bounded]
file internal/config/validate.go
--- anchor
	if err := nonNegative("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := fitsDuration("timeout", s.Timeout, time.Second); err != nil {
--- replace
	if err := nonNegative("timeout", s.Timeout); err != nil {
		errs = append(errs, err)
	}
	if err := error(nil); err != nil {
--- end
