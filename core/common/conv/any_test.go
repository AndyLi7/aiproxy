package conv_test

import (
	"testing"

	"github.com/labring/aiproxy/core/common/conv"
	"github.com/smartystreets/goconvey/convey"
)

func TestBytesToString(t *testing.T) {
	convey.Convey("BytesToString", t, func() {
		convey.Convey("should convert bytes to string", func() {
			b := []byte("hello")
			s := conv.BytesToString(b)
			convey.So(s, convey.ShouldEqual, "hello")
		})
	})
}

func TestStringToBytes(t *testing.T) {
	convey.Convey("StringToBytes", t, func() {
		convey.Convey("should convert string to bytes", func() {
			s := "hello"
			b := conv.StringToBytes(s)
			convey.So(b, convey.ShouldResemble, []byte("hello"))
		})
	})
}

func TestConversionsDoNotAlias(t *testing.T) {
	source := []byte("original")
	text := conv.BytesToString(source)
	source[0] = 'X'

	if text != "original" {
		t.Fatal("string changed when source bytes were mutated")
	}

	bytes := conv.StringToBytes(text)
	bytes[0] = 'Y'

	if text != "original" {
		t.Fatal("source string changed when converted bytes were mutated")
	}

	second := conv.StringToBytes(text)
	if string(second) != "original" {
		t.Fatal("conversions share mutable storage")
	}

	literal := conv.StringToBytes("literal")

	literal[0] = 'L'
	if string(literal) != "Literal" {
		t.Fatal("converted literal is not writable")
	}
}
