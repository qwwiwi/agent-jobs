package guard

import (
	"path/filepath"
	"testing"
)

// #54 fix loop 1: atool and aunpack bundle short options like getopt (Getopt::Long bundling), so a
// directory option inside a cluster (`-fX<dir>`, `-xfX<dir>`) names the extraction directory too.
func TestProtectedAncestorBundledDirSwitch(t *testing.T) {
	state, _, _, cfg := protectedSetup(t)
	home := filepath.Dir(filepath.Dir(state))
	up := "../../../.."
	for _, c := range []string{
		"atool -fX" + home + " payload.tar",
		"atool -fX " + home + " payload.tar",
		"aunpack -fX" + home + " payload.tar",
		"aunpack -fX " + home + " payload.tar",
		"atool -xfX" + state + " payload.tar",
		"atool -qfX" + up + " payload.tar",
		"aunpack -fqX" + up + " payload.tar",
		"atool -x -fX" + up + " payload.tar",
		"/usr/bin/AUNPACK -fX" + home + " payload.tar",
		// a numeric -V hands the rest back as a new cluster
		"atool -V1X" + up + " payload.tar",
		"atool --extract-to=" + home + " payload.tar",
		"atool --extract-to " + up + " payload.tar",
		"aunpack --extract-to=" + up + " payload.tar",
	} {
		if Check(c, cfg) == "" {
			t.Errorf("%q allowed", c)
		}
	}
	for _, c := range []string{
		"atool -fXbuild payload.tar", "aunpack -fXbuild payload.tar", "atool -xfXbuild payload.tar",
		"atool -fX build payload.tar", "aunpack -fqX.. payload.tar", "aunpack -Xbuild a.zip",
		// a value option ends the cluster: the rest is its value, not a directory
		"atool -lF/x payload", "aunpack -fFX/x payload.tar", "atool -xO-X/ payload.tar",
		// lower-case x is --extract, not --extract-to
		"atool -x payload.tar", "atool -fx payload.tar",
	} {
		if r := Check(c, cfg); r != "" {
			t.Errorf("%q refused: %s", c, r)
		}
	}
}
