package detect

import "testing"

// TestDetectPytestMarkers checks that a Python project with pytest tests is never left
// to unittest, which would run none of them.
func TestDetectPytestMarkers(t *testing.T) {
	tc := "import unittest\n\nclass T(unittest.TestCase):\n    def test_a(self):\n        pass\n"
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"plain unittest", map[string]string{"pyproject.toml": "[project]", "tests/test_a.py": tc}, "unittest"},
		// Manifests, lockfiles and task files mentioning pytest.
		{"dev-requirements.txt", map[string]string{"pyproject.toml": "", "dev-requirements.txt": "pytest==8\n", "tests/test_a.py": tc}, "pytest"},
		{"test-requirements.in", map[string]string{"setup.py": "", "test-requirements.in": "pytest-cov\n", "tests/test_a.py": tc}, "pytest"},
		{"requirements/ dir", map[string]string{"setup.cfg": "", "requirements/dev.txt": "pytest\n", "tests/test_a.py": tc}, "pytest"},
		{"nested requirements/ dir", map[string]string{"setup.cfg": "", "ci/requirements/test.in": "pytest\n", "tests/test_a.py": tc}, "pytest"},
		{"requirements without pytest", map[string]string{"setup.cfg": "", "requirements/base.txt": "requests\n", "requirements-dev.txt": "black\n", "tests/test_a.py": tc}, "unittest"},
		{"requirements above the project", map[string]string{"requirements-dev.txt": "pytest\n", "svc/pyproject.toml": "", "svc/tests/test_a.py": tc}, "pytest"},
		{"noxfile.py", map[string]string{"pyproject.toml": "", "noxfile.py": "session.run(\"pytest\")\n", "tests/test_a.py": tc}, "pytest"},
		{"Pipfile", map[string]string{"setup.py": "", "Pipfile": "[dev-packages]\npytest = \"*\"\n", "tests/test_a.py": tc}, "pytest"},
		{"poetry.lock", map[string]string{"pyproject.toml": "", "poetry.lock": "[[package]]\nname = \"pytest\"\n", "tests/test_a.py": tc}, "pytest"},
		{"pdm.lock", map[string]string{"pyproject.toml": "", "pdm.lock": "name = \"pytest\"\n", "tests/test_a.py": tc}, "pytest"},
		{"uv.lock", map[string]string{"pyproject.toml": "", "uv.lock": "name = \"pytest\"\n", "tests/test_a.py": tc}, "pytest"},
		{"workflow runs pytest", map[string]string{"pyproject.toml": "", ".github/workflows/ci.yml": "steps:\n  - run: python -m pytest -q\n", "tests/test_a.py": tc}, "pytest"},
		{"workflow runs unittest", map[string]string{"pyproject.toml": "", ".github/workflows/ci.yaml": "steps:\n  - run: python -m unittest\n", "tests/test_a.py": tc}, "unittest"},
		// Test files written for pytest.
		{"module-level test function", map[string]string{"pyproject.toml": "", "tests/test_a.py": tc, "tests/test_b.py": "def test_b():\n    assert 1\n"}, "pytest"},
		{"async test function", map[string]string{"pyproject.toml": "", "tests/test_b.py": "async def test_b():\n    pass\n"}, "pytest"},
		{"pytest-style x_test.py", map[string]string{"pyproject.toml": "", "tests/test_a.py": tc, "tests/b_test.py": "def test_b():\n    pass\n"}, "pytest"},
		{"import pytest", map[string]string{"pyproject.toml": "", "tests/test_b.py": "import pytest\n\nclass TestX:\n    def test_x(self):\n        pass\n"}, "pytest"},
		{"from pytest import", map[string]string{"pyproject.toml": "", "tests/test_b.py": "from pytest import raises\n" + tc}, "pytest"},
		{"test functions next to a TestCase", map[string]string{"pyproject.toml": "", "tests/test_b.py": tc + "\n\ndef test_c():\n    pass\n"}, "pytest"},
		{"TestCase methods are not module-level functions", map[string]string{"pyproject.toml": "", "tests/test_a.py": tc + "\n    def test_b(self):\n        pass\n"}, "unittest"},
		{"test function in a file that is not a test", map[string]string{"pyproject.toml": "", "lib/util.py": "def test_util():\n    pass\n", "tests/test_a.py": tc}, "unittest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, _ := detect(t, c.files)
			if len(g) != 1 || g[0].Runner != c.want {
				t.Errorf("got %v, want one %s suite", g, c.want)
			}
		})
	}
}
