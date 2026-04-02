#!/bin/bash
echo "Debugging FoxTrack Bridge..."

echo "1. Checking if server is running:"
ps aux | grep foxtrack-bridge

echo ""
echo "2. Testing API endpoint:"
curl -v http://localhost:8080/api/status

echo ""
echo "3. Checking network connectivity to printer:"
ping -c 3 192.168.1.100

echo ""
echo "4. Checking if port 1883 is accessible:"
timeout 5 telnet 192.168.1.100 1883
