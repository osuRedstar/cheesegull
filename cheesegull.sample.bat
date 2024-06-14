@echo off
B:
cd B:\redstar\cheesegull
cheesegull.exe -k Your_Bancho_APIKKEY -u Your_Bancho_username -p Your_Bancho_password -m root:@(127.0.0.1:3306)/cheesegull --search-dsn root:@(127.0.0.1:3306)/cheesegull -a 0.0.0.0:6201 --secret-ci Your_CIKey  --folders="/data/"
pause