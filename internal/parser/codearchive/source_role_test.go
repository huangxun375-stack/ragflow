package codearchive

import "testing"

func TestIsTestSource(t *testing.T) {
	for _, p := range []string{
		"driver_test.go", "test_driver.py", "driver_test.py", "conftest.py", "test/helpers.py", "tests/helpers.py",
		"a.test.js", "a.spec.ts", "a.test.unit.ts", "a.spec.integration.tsx", "a.test.jsx", "a.spec.tsx", "a.test.vue", "__tests__/helper.ts", "__mocks__/fetch.js", "test/helper.vue", "tests/helper.jsx",
		"src/test/java/Helper.java", "a/src/test/Helper.java", "DriverTest.java", "DriverTests.java", "DriverIT.java",
		"test/helper.c", "tests/helper.hpp", "driver_test.cpp", "driver_test.cxx", "driver_unittest.cc", "test_driver.c",
		"spec/helper.lua", "driver_spec.lua", "test_driver.lua", "tests/helper.lua", `src\test\java\Helper.java`,
	} {
		t.Run(p, func(t *testing.T) {
			if !IsTestSource(p) {
				t.Fatal("test source not recognized")
			}
		})
	}
	for _, p := range []string{"", "latest.go", "contest/helper.go", "contest/helper.py", "contest/helper.ts", "tests.go", "test.go", "tests/driver.go", "src/contest/Helper.java", "testimony.java", "unit/Latest.java", "a.test.txt", "a.test.js.map", "tests/readme.md", "spectator/a.lua", "contest/a.c", "mytests/a.ts", "a.testimony.js", "a_spec.go", "test_driver.cpp", "test/helper.lua"} {
		t.Run(p, func(t *testing.T) {
			if IsTestSource(p) {
				t.Fatal("production source misclassified")
			}
		})
	}
}
