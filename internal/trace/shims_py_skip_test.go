package trace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// A test file a test of which skipped itself at run time (pytest.skip() in a test or a
// fixture, a skipif condition, importorskip, a module-level skip) is not proven to pass:
// "unknown", whether all or some of its tests skipped. A test disabled by an
// unconditional skip mark, an expected failure and a test deselected with -k keep it
// passing; a failure wins.
func TestPytestRuntimeSkips(t *testing.T) {
	root := shimRepo(t, map[string]string{
		"test_static.py":       "import pytest\n\n\n@pytest.mark.skip(reason='off')\ndef test_off():\n    pass\n\n\ndef test_on():\n    pass\n",
		"test_pytestmark.py":   "import pytest\n\npytestmark = pytest.mark.skip(reason='off')\n\n\ndef test_off():\n    pass\n",
		"test_skipif.py":       "import pytest\n\n\n@pytest.mark.skipif(True, reason='env')\ndef test_cond():\n    pass\n\n\ndef test_on():\n    pass\n",
		"test_skipif_off.py":   "import pytest\n\n\n@pytest.mark.skipif(False, reason='env')\ndef test_cond():\n    pass\n",
		"test_call.py":         "import pytest\n\n\ndef test_call():\n    pytest.skip('no server')\n",
		"test_fixture.py":      "import pytest\n\n\n@pytest.fixture\ndef db():\n    pytest.skip('no db')\n\n\ndef test_db(db):\n    pass\n\n\ndef test_on():\n    pass\n",
		"test_module.py":       "import pytest\n\npytest.skip('not here', allow_module_level=True)\n\n\ndef test_x():\n    pass\n",
		"test_importorskip.py": "import pytest\n\npytest.importorskip('smates_no_such_module')\n\n\ndef test_x():\n    pass\n",
		"test_xfail.py":        "import pytest\n\n\n@pytest.mark.xfail(reason='known')\ndef test_known():\n    assert False\n",
		"test_deselect.py":     "def test_keep():\n    pass\n\n\ndef test_other_deselected():\n    pass\n",
		"test_failskip.py":     "import pytest\n\n\ndef test_bad():\n    assert False\n\n\ndef test_skip():\n    pytest.skip('x')\n",
	})
	_, status := tracePytestRun(t, root, "-k", "not deselected")
	for f, want := range map[string]string{
		"test_static.py": state.Pass, "test_pytestmark.py": state.Pass, "test_skipif.py": state.Unknown,
		"test_skipif_off.py": state.Pass, "test_call.py": state.Unknown, "test_fixture.py": state.Unknown,
		"test_module.py": state.Unknown, "test_importorskip.py": state.Unknown, "test_xfail.py": state.Pass,
		"test_deselect.py": state.Pass, "test_failskip.py": state.Fail,
	} {
		if status[f] != want {
			t.Errorf("%s: status %q, want %q", f, status[f], want)
		}
	}
}

// unittest under smates_unittest: a test that skipped itself (skipTest, SkipTest raised
// in a test, setUpClass or at import, a skipIf/skipUnless whose condition held, also
// imported by name) makes its module unknown; an unconditional @unittest.skip (on a
// method or a class) does not, nor does a condition that did not hold.
func TestUnittestRuntimeSkips(t *testing.T) {
	py := testPython(t)
	cls := func(body string) string {
		return "import unittest\n\n\nclass T(unittest.TestCase):\n" + body + "\n    def test_on(self):\n        pass\n"
	}
	root := shimRepo(t, map[string]string{
		"test_static.py":     cls("    @unittest.skip('off')\n    def test_off(self):\n        pass\n"),
		"test_classskip.py":  "import unittest\n\n\n@unittest.skip('off')\nclass T(unittest.TestCase):\n    def test_a(self):\n        pass\n",
		"test_skipif.py":     cls("    @unittest.skipIf(True, 'env')\n    def test_cond(self):\n        pass\n"),
		"test_skipif_off.py": cls("    @unittest.skipIf(False, 'env')\n    def test_cond(self):\n        pass\n"),
		"test_skipunless.py": cls("    @unittest.skipUnless(False, 'env')\n    def test_cond(self):\n        pass\n"),
		"test_imported.py":   "import unittest\nfrom unittest import skipIf\n\n\nclass T(unittest.TestCase):\n    @skipIf(True, 'env')\n    def test_cond(self):\n        pass\n",
		"test_skiptest.py":   cls("    def test_self(self):\n        self.skipTest('no server')\n"),
		"test_setupclass.py": "import unittest\n\n\nclass T(unittest.TestCase):\n    @classmethod\n    def setUpClass(cls):\n        raise unittest.SkipTest('no db')\n\n    def test_a(self):\n        pass\n",
		"test_module.py":     "import unittest\n\nraise unittest.SkipTest('not here')\n",
		"test_subtest.py":    cls("    def test_sub(self):\n        with self.subTest(i=1):\n            self.skipTest('x')\n"),
		"test_failskip.py":   cls("    def test_bad(self):\n        self.fail('x')\n\n    def test_skip(self):\n        self.skipTest('x')\n"),
	})
	sh, traceDir := extractShims(t)
	mods := []string{"test_static", "test_classskip", "test_skipif", "test_skipif_off", "test_skipunless", "test_imported",
		"test_skiptest", "test_setupclass", "test_module", "test_subtest", "test_failskip"}
	out, code := runPy(t, root, PytestEnv(traceDir, root, sh, os.Environ()), append([]string{py, "-m", "smates_unittest"}, mods...)...)
	t.Logf("exit %d:\n%s", code, out)
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for id, u := range Attribute(procs, unitOfPy, Options{}).Units {
		status[id] = u.Status
	}
	for f, r := range Results(traceDir, root) {
		status[f] = r.Status
	}
	for f, want := range map[string]string{
		"test_static.py": state.Pass, "test_classskip.py": state.Pass, "test_skipif.py": state.Unknown,
		"test_skipif_off.py": state.Pass, "test_skipunless.py": state.Unknown, "test_imported.py": state.Unknown,
		"test_skiptest.py": state.Unknown, "test_setupclass.py": state.Unknown, "test_module.py": state.Unknown,
		"test_subtest.py": state.Unknown, "test_failskip.py": state.Fail,
	} {
		if status[f] != want {
			t.Errorf("%s: status %q, want %q", f, status[f], want)
		}
	}
}

// A path a Python process reads through a symlink of the repository that points outside
// of it records the link.
func TestPythonShimSymlinkOutside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root := shimRepo(t, map[string]string{
		"t.py":        "import os\nopen('extdir/f.txt').read()\nopen('extfile').read()\nos.listdir('extdir')\nopen('data/in.txt').read()\n",
		"data/v1.txt": "",
	})
	out := shimRepo(t, map[string]string{"ext/f.txt": "", "v2.txt": ""})
	for link, target := range map[string]string{
		"extdir":      filepath.Join(out, "ext"),
		"extfile":     filepath.Join(out, "v2.txt"),
		"data/in.txt": "v1.txt",
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	reads := runPyTraced(t, root, "t.py")
	checkDeps(t, reads, []string{"extdir", "extfile", "data/in.txt", "data/v1.txt"}, nil)
}
