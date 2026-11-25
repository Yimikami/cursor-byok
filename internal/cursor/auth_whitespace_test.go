package cursor

import (
	"reflect"
	"testing"
)

func TestLegacyAuthWithWhitespaceRestoresRealBackup(t *testing.T) {
	original := map[string]string{"cursorAuth/accessToken": "original-owner", "cursorAuth/refreshToken": "original-refresh", "unrelated": "keep"}
	db, path, backup := testCursorDB(t, original)
	if err := injectFakeProUserAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE ItemTable SET value = ? WHERE key = 'cursorAuth/accessToken'", " \n"+fakeProSessionJWT+"\t"); err != nil {
		t.Fatal(err)
	}
	if err := prepareCursorAuthAt(path, backup); err != nil {
		t.Fatal(err)
	}
	if got := readTestItems(t, db); !reflect.DeepEqual(got, original) {
		t.Fatal("valid account backup not restored")
	}
}
