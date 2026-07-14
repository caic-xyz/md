# DBus session environment created by container startup.
# shellcheck shell=bash

dbus_session_env="${HOME}/.dbus-session-env"
if [ -r "$dbus_session_env" ]; then
	# shellcheck disable=SC1090
	. "$dbus_session_env"
	export DBUS_SESSION_BUS_ADDRESS
fi
unset dbus_session_env
