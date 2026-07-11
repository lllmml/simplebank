package val

import (
	"fmt"
	"net/mail"
	"regexp"
)
// "^" 表示字符串的开头 "[]" 用来列出想要在字符串中包含的所有可能字符， "+"表示方括号内的任何字符都能出现一次或多次
// "$"表示字符串的结尾
// 在go语言中，使用双反斜杠后跟一个s，表示任何空格字符 \s
var (
	isValidUsername = regexp.MustCompile(`^[a-z0-9_]+$`).MatchString   //是个函数
	isValidFullName = regexp.MustCompile(`^[a-z0-9A-Z\s]+$`).MatchString  //是个函数
)

func ValidateString(value string, minLength int, maxLength int) error {
	n := len(value)
	if n < minLength || n > maxLength {
		return fmt.Errorf("must contain from %d-%d characters", minLength, maxLength)
	}
	return nil
}

func ValidateUsername(value string) error {
	if err := ValidateString(value, 3, 100); err != nil {
		return err
	}
	if !isValidUsername(value) {
		return fmt.Errorf("must contain only lowercase letters, digits, or underscore")
	}
	return nil
}

func ValidateFullName(value string) error {
	if err := ValidateString(value, 3, 100); err != nil {
		return err
	}
	if !isValidFullName(value) {
		return fmt.Errorf("must contain only letters or spaces")
	}
	return nil
}

func ValidatePassword(value string) error {
	return ValidateString(value, 6, 100)
}

func ValidateEmail(value string) error {
	if err := ValidateString(value, 3, 200); err != nil {
		return err
	}
	if _, err := mail.ParseAddress(value); err != nil {
		return fmt.Errorf("is not a valid email address")
	}
	return nil
}

func ValidateEmailId(value int64) error {
	if value <= 0 {
		return fmt.Errorf("must be a positive integer")
	}
	return nil
}

func ValidateSecretCode(value string) error {
	return ValidateString(value, 32, 128)
}