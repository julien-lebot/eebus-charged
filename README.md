# EEBUS Charged

Standalone EEBUS-based charger control system for controlling EV chargers.

## Features

- Direct EEBUS communication with compatible chargers (Porsche Mobile Charger Connect, Elli, etc.)
- **Smart protocol handling** - automatically detects and uses the best charging protocol:
  - IEC 61851 basic mode
  - ISO 15118-2 with VW/Porsche VAS support for seamless pause/resume
- **Auto-stop on vehicle connect** - prevents unwanted charging until you're ready
- Start, stop, and pause charging via API/CLI
- Set charging current dynamically
- Vehicle identification (ISO 15118)
- Multi-charger support with auto-discovery
- Command-line interface
- HTTP API for integration with home automation
- **MQTT state publishing** for Home Assistant and other automation systems
- **Datadog APM tracing** for production monitoring

## Quick Start

### Prerequisites

- **Go 1.22 or later** - Download from [golang.org](https://golang.org/dl/)
- Verify: `go version`

### 1. Build

```bash
cd marignier/charged/eebus-charged
go mod download
go build
# Windows: creates eebus-charged.exe
# Linux/Mac: go build -o eebus-charged
```

### 2. Configure

Edit `config.yaml`:

```yaml
# Required: Your infrastructure limits
charging:
  min_current: 6.0   # Minimum safe for most EVs
  max_current: 28.0  # YOUR house breaker limit (adjust this!)
  phases: 3

# Optional: Only needed if mDNS auto-discovery doesn't work
# chargers:
#   - name: "garage_wallbox"
#     ski: "abcd-1234-5678-90ef-1234-5678-90ab-cdef-1234-5678"
#     ip: "10.10.10.58"
```

**Note**: Chargers auto-discover by default. No manual config needed!

### 3. Run

```bash
# Start the service
./eebus-charged run
```

The service will:
- Start EEBUS on port 4712
- Start control API on port 8080
- Auto-discover and auto-pair with any chargers
- Detect vehicle connections
- Log charger SKI for manual config (if needed)

### 4. Pair Your Charger

Put your charger in pairing mode (via the charger's app). Watch the HEMS logs:

```
INFO  Pairing request received - auto-accepting  {"ski": "abcd1234..."}
INFO  Remote device connected  {"ski": "abcd1234..."}
INFO  Auto-discovered charger  {"name": "charger_12345678", "ski": "abcd1234..."}
INFO  Charger added  {"name": "charger_12345678"}
```

**When you plug in your vehicle**, it will automatically **stop charging**:

```
INFO  Vehicle connected  {"vehicle_id": "AA:BB:CC:DD:EE:FF"}
INFO  Vehicle connected - charging stopped (call start to begin)
```

This prevents unwanted charging during peak hours. You must explicitly call `start` to begin charging.

### 5. Control Charging

In another terminal, use the auto-generated name:

```bash
# Check status (shows all connected chargers)
./eebus-charged control status

# Start charging (use the name from logs)
./eebus-charged control start --charger charger_12345678

# Set current to 16A
./eebus-charged control set-current --charger charger_12345678 --current 16.0

# Stop charging
./eebus-charged control stop --charger charger_12345678
```

### 6. Optional: Set a Custom Name

Add to `config.yaml`:

```yaml
chargers:
  - name: "garage_wallbox"  # Your preferred name
    ski: "abcd-1234-5678-90ef-1234-5678-90ab-cdef-1234-5678"
```

Restart the service. Now use:

```bash
./eebus-charged control start --charger garage_wallbox
```

## Architecture

```
eebus-charged run  →  EEBUS Service (port 4712)
                      Control API (port 8080)
                      MQTT Publisher (optional)
                      Datadog APM (optional)
                           ↑
eebus-charged control → HTTP calls to API → Charger control
                           ↓
                      MQTT Topics → Home Assistant/automation
                      Datadog Traces → Observability
```

## Configuration

### Service Section
```yaml
service:
  brand: "Custom HEMS"
  model: "EEBUS-CHARGED v1.0"
  serial: "HEMS-001"  # Must be unique
  device_name: "Home Energy Manager"
```

### Network Section
```yaml
network:
  port: 4712      # EEBUS communication port
  api_port: 8080  # HTTP control API port
  interface: ""   # Empty = all interfaces
```

### Chargers Section (Optional)

**By default**: Chargers auto-discover via mDNS - no config needed!

**Only configure if**:
- mDNS doesn't work (firewall/network issues)
- You want custom names instead of auto-generated ones

```yaml
chargers:
  - name: "garage_wallbox"  # Your custom name
    ski: "abcd-1234-5678-90ef-1234-5678-90ab-cdef-1234-5678"
    ip: "10.10.10.58"  # For direct IP connection
```

**To find SKI**: Check the HEMS logs when charger first connects

### Charging Section
```yaml
charging:
  min_current: 6.0       # Minimum safe charging current
  max_current: 28.0      # Your electrical capacity limit
  default_current: 16.0  # Default when starting
  phases: 3              # 1 or 3
```

### Logging Section
```yaml
logging:
  level: "info"      # debug, info, warn, error
  format: "console"  # console or json
  file: "log.txt"    # Optional: log file path (empty = console only)
```

**Note**: If `file` is set, logs go **only** to the file, not to console. Set to empty string for console logging.

### Datadog Section
```yaml
datadog:
  enabled: false                # Enable Datadog APM tracing
  agent_host: "localhost"       # Datadog agent host (container name in Docker)
  agent_port: 8126              # Datadog trace agent port
  service_name: "eebus-charged"    # Service name in Datadog
  environment: "production"     # Environment tag
```

**Production Setup**: When running in Docker with Datadog agent:
```yaml
datadog:
  enabled: true
  agent_host: "datadog-agent"  # Docker container name
  agent_port: 8126
  service_name: "eebus-charged"
  environment: "production"
```

Datadog will automatically trace:
- EEBUS operations (pairing, charging control)
- HTTP API requests
- MQTT publishing
- All charger operations (start, stop, set current)

### MQTT Section
```yaml
mqtt:
  enabled: false                # Enable MQTT state publishing
  broker: "localhost"           # MQTT broker host
  port: 1883                    # MQTT broker port
  username: ""                  # Optional authentication
  password: ""                  # Optional authentication
  client_id: "eebus-charged"       # MQTT client ID
  topic_prefix: "hems"          # Topic prefix (hems/chargers/...)
```

**For Home Assistant**:
```yaml
mqtt:
  enabled: true
  broker: "homeassistant.local"  # Your HA host
  port: 1883
  username: "mqtt_user"
  password: "mqtt_password"
  topic_prefix: "hems"
```

**Published Topics** (for charger named `garage_wallbox`):
- `hems/chargers/garage_wallbox/connected` - `true`/`false`
- `hems/chargers/garage_wallbox/charging` - `true`/`false`
- `hems/chargers/garage_wallbox/vehicle_identity` - Vehicle MAC address
- `hems/chargers/garage_wallbox/vehicle_name` - Vehicle identifier
- `hems/chargers/garage_wallbox/charge_power` - Current power (W)
- `hems/chargers/garage_wallbox/current_limit` - Current limit (A)
- `hems/chargers/garage_wallbox/state` - Complete state as JSON

**What ISO 15118-2 Does NOT Provide:**
- **Vehicle SoC** - Not available in ISO 15118-2 (used by Porsche Macan). Only ISO 15118-20 (newer standard) can provide SoC, and it's optional.
- **Session Energy** - The protocol doesn't track cumulative energy per session. You would need to integrate power measurements yourself over time.
- **Charge Remaining Energy** - Calculated value that requires SoC, which isn't available.

These topics/fields are published for compatibility with Home Assistant but will always be `0` or `null`.

## Deployment

### Home Assistant Integration

Add to your Home Assistant `configuration.yaml`:

**Control Options:**

**Option 1: Simple Buttons**:

```yaml
mqtt:
  button:
    # Start charging button
    - name: "EV Charger Start"
      unique_id: "ev_charger_start"
      command_topic: "hems/chargers/garage_wallbox/command"
      payload_press: "start"
      icon: "mdi:play"
    
    # Stop charging button
    - name: "EV Charger Stop"
      unique_id: "ev_charger_stop"
      command_topic: "hems/chargers/garage_wallbox/command"
      payload_press: "stop"
      icon: "mdi:stop"
```

**Option 2: Switch with State** (shows charging status, no automations needed):

```yaml
mqtt:
  switch:
    - name: "EV Charger Control"
      unique_id: "ev_charger_control"
      state_topic: "hems/chargers/garage_wallbox/charging"
      command_topic: "hems/chargers/garage_wallbox/command"
      payload_on: "start"
      payload_off: "stop"
      state_on: "true"
      state_off: "false"
      icon: "mdi:ev-station"
```

The switch will:
- Accurately reflect the charging state from `hems/chargers/{name}/charging` topic
- Send "start" when turned on, "stop" when turned off
- No automations needed - works directly with Home Assistant's MQTT switch
  
  # Optional: Status sensors
  sensor:
    - state_topic: "hems/chargers/garage_wallbox/vehicle_identity"
      name: "EV Charger Vehicle Identity"
      object_id: "ev_charger_vehicle_identity"
    
    - state_topic: "hems/chargers/garage_wallbox/charge_power"
      name: "EV Charger Power"
      object_id: "ev_charger_power"
      unit_of_measurement: "W"
      device_class: power
    
    - state_topic: "hems/chargers/garage_wallbox/current_limit"
      name: "EV Charger Current Limit"
      object_id: "ev_charger_current_limit"
      unit_of_measurement: "A"
      device_class: current
    
    - state_topic: "hems/chargers/garage_wallbox/session_energy"
      name: "EV Charger Session Energy"
      object_id: "ev_charger_session_energy"
      unit_of_measurement: "Wh"
      device_class: energy
      state_class: total_increasing

  number:
    - name: "EV Charger Current Limit"
      unique_id: "ev_charger_current_limit_control"
      state_topic: "hems/chargers/garage_wallbox/current_limit"
      command_topic: "hems/chargers/garage_wallbox/command"
      min: 1
      max: 32
      step: 0.1
      unit_of_measurement: "A"
      device_class: current
      icon: "mdi:current-ac"

  binary_sensor:
    - name: "EV Charger Vehicle Connected"
      object_id: "ev_charger_vehicle_connected"
      state_topic: "hems/chargers/garage_wallbox/connected"
      payload_on: "true"
      payload_off: "false"
    
    - name: "EV Charger Charging"
      object_id: "ev_charger_charging"
      state_topic: "hems/chargers/garage_wallbox/charging"
      payload_on: "true"
      payload_off: "false"
```

You can then create automations based on these sensors, e.g.:
- Start charging when solar production is high
- Stop charging before peak hours
- Send notifications when vehicle connects
- Track energy consumption

### Docker

```bash
docker build -t eebus-charged .
docker-compose up -d
```

### Systemd Service (Linux)

```bash
# Install
sudo cp eebus-charged /opt/eebus-charged/
sudo cp config.yaml /opt/eebus-charged/
sudo cp eebus-charged.service /etc/systemd/system/
sudo systemctl enable eebus-charged
sudo systemctl start eebus-charged

# View logs
sudo journalctl -u eebus-charged -f
```

## Troubleshooting

### Service won't start
- Check Go is installed: `go version`
- Enable debug logging in `config.yaml`: `logging.level: "debug"`
- Check port 4712 is not in use
- Check API port (default 8080) is not in use

### Charger won't connect
- Verify SKI in configuration
- Check network connectivity: `ping CHARGER-IP`
- Ensure firewall allows port 4712 (TCP/UDP)
- Put charger in pairing mode

### Control commands fail
- Make sure service is running: `./eebus-charged run`
- Check API is accessible: `curl http://localhost:8080/api/chargers`
- Use `--api` flag if running on different address

## API Endpoints

The control API runs on `localhost:8080` (configurable via `network.api_port`):

- `GET /api/chargers` - List all chargers
- `GET /api/chargers/{name}/status` - Get charger status
- `POST /api/chargers/{name}/start` - Start charging
- `POST /api/chargers/{name}/stop` - Stop charging
- `PUT /api/chargers/{name}/current` - Set current limit (JSON body: `{"current": 16.0}`)

## License

MIT
