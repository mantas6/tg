- `pull` command: should sync only todays entries, but if specified with -a/--all flag it should sync all entries from this month (start to end)
- new command `daily`: returns sum of time tracked per day starting from this month start and ending in this months end. It should return a list of tracked time per day + footer with sum. There should be a parameter -t/--target with default value of 8 (hours), meaning the target hours worked per day. So each list day and total should show how much there is overtime in minutes or hours:minutes.
- `add` command: if start time is not specified, it adds a new entry with start_time of last entry's end_time. Syntax `tg add 1:30 "test"`

remove item from this list once implemented
