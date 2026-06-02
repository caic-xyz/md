# Interactive shell aliases.
# shellcheck shell=bash

case $- in
*i*) ;;
*) return ;;
esac

alias ll='ls --color=auto -la'
alias vimdiff='nvim -d'
