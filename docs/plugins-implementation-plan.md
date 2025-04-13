# Navidrome Plugin System Implementation Plan

## Progress Tracking

### Phase 1: Foundational Infrastructure

- [ ] 1.1: Plugin Manifest and Configuration
- [ ] 1.2: Basic WebAssembly Runtime Integration
- [ ] 1.3: Permission Management System
  - [ ] 1.3.1: URL Allowlist Implementation
  - [ ] 1.3.2: Local Network Access Control
  - [ ] 1.3.3: Host Function Access Control
- [ ] 1.4: Project Structure and CLI Commands
- [ ] 1.5: Plugin Verification System

### Phase 2: Protocol Definition and Host Functions

- [ ] 2.1: Protocol Buffer Definitions
- [ ] 2.2: Host Function Implementation
- [ ] 2.3: Plugin Context Management

### Phase 3: Plugin Loading and Execution

- [ ] 3.1: WebAssembly Runtime Configuration
- [ ] 3.2: Testing Infrastructure
- [ ] 3.3: Plugin Developer Tools

### Phase 4: Agent Plugin Integration

- [ ] 4.1: Agent Plugin Adapter Implementation
- [ ] 4.2: Plugin Registration with Agent System
- [ ] 4.3: Last.fm Agent Plugin Implementation
- [ ] 4.4: Integration Testing

### Phase 5: Enhanced Management and User Experience

- [ ] 5.1: Enhanced CLI Management
- [ ] 5.2: Plugin Package Format
- [ ] 5.3: Runtime Monitoring
- [ ] 5.4: Administrative UI (Optional)

### Phase 6: Documentation and Release

- [ ] 6.1: User Documentation
- [ ] 6.2: Developer Documentation
- [ ] 6.3: Example Plugin Templates
- [ ] 6.4: Final Testing and Feature Flags

## Phase 1: Foundational Infrastructure

**Goal:** Establish the core plugin infrastructure without affecting existing functionality.

### 1.1: Plugin Manifest and Configuration

- Create plugin manifest schema and validation functions
- Add plugin-related configuration to `conf` package:
  - Global plugin settings: enabled, directory, default limits
  - Per-plugin settings: enabled, limits, configuration
- Add tests for manifest validation and configuration parsing

### 1.2: Basic WebAssembly Runtime Integration

- Add `knqyf263/go-plugin` dependency
- Create initial plugin loader that can:
  - Discover plugin files in configured directory
  - Read and validate manifests
  - Basic security validation (no plugin execution yet)
- Add unit tests for plugin discovery and manifest loading

### 1.3: Permission Management System

- Implement the `PermissionManager` component:
  - URL allowlist validation
  - Host function allowlist validation
  - Internal network access prevention
  - Configuration access control
- Add comprehensive security tests for all permission rules
- Implement local network access control feature:
  - Add `allowLocalNetwork` flag to manifest schema
  - Update permission checks in HTTP requests
  - Add configuration option for default behavior
- Add tests for local network access control

### 1.4: Project Structure and CLI Commands

- Create plugin-related directory structure:
  ```
  plugins/
  ├── proto/       # Protocol Buffer definitions
  ├── manager.go   # Plugin Manager implementation
  ├── host.go      # Host function implementations
  ├── permission.go # Permission manager
  └── adapters/    # Adapters for different plugin types
  ```
- Implement basic CLI commands for plugin management:
  - `navidrome plugin list`
  - `navidrome plugin info [name]`

### 1.5: Plugin Verification System

- Implement plugin binary integrity verification:
  - Add hash calculation and storage during installation
  - Add verification during plugin loading
  - Create a local store for plugin hashes
- Add tests for plugin verification workflow
- Update CLI commands to display verification status

**Deliverable:** Foundation layer with security features including local network control and plugin verification.

## Phase 2: Protocol Definition and Host Functions

**Goal:** Define the communication protocol between Navidrome and plugins.

### 2.1: Protocol Buffer Definitions

- Define Protocol Buffer specifications for:
  - Agent plugin interface
  - Host functions interface
  - Common request/response structures
- Generate Go code from Protocol Buffers
- Create test stubs for interface implementations

### 2.2: Host Function Implementation

- Implement core host functions:
  - `GetConfig` for configuration access
  - `Log` for plugin logging
  - `HttpDo` for controlled HTTP access
- Add comprehensive tests for each host function
- Implement permission checks for all host functions

### 2.3: Plugin Context Management

- Create plugin context structure to track:
  - Current plugin name
  - Permission scope
  - Runtime state
- Implement proper isolation between plugin calls

**Deliverable:** Complete protocol definition and host function implementations without executing actual plugins.

## Phase 3: Plugin Loading and Execution (Minimal)

**Goal:** Enable basic plugin loading and execution in isolation from the rest of the system.

### 3.1: WebAssembly Runtime Configuration

- Configure WebAssembly runtime with appropriate security settings
- Implement plugin initialization with configuration passing
- Add proper error handling for plugin loading failures

### 3.2: Testing Infrastructure

- Create test harness for plugin execution
- Implement simple test plugins for validation
- Add integration tests for plugin loading and execution
- Add tests for local network access
- Add tests for plugin verification and integrity checks

### 3.3: Plugin Developer Tools

- Implement development commands:
  - `navidrome plugin dev [folder_path]`
  - `navidrome plugin refresh [name]`
- Create basic development documentation

**Deliverable:** Working plugin loading and execution system that can be tested in isolation.

## Phase 4: Agent Plugin Integration

**Goal:** Connect the plugin system to the existing agent architecture.

### 4.1: Agent Plugin Adapter Implementation

- Create adapter that implements all agent interfaces:
  - Convert between Protobuf and agent interfaces
  - Implement proper error handling and timeouts
  - Add trace logging for debugging
- Add unit tests for all adapter methods
- Update adapter to respect plugin's declared capabilities

### 4.2: Plugin Registration with Agent System

- Implement plugin registration with the existing agent system
- Extend configuration to support plugin agent ordering
- Make plugin agents respect the same priority system as built-in agents

### 4.3: Last.fm Agent Plugin Implementation

- Implement prototype Last.fm plugin as proof of concept
- Create plugin manifest with necessary permissions
- Add tests comparing plugin behavior to built-in agent

### 4.4: Integration Testing

- Add comprehensive integration tests for:
  - Plugin discovery and loading
  - Agent API functionality
  - Error handling and recovery
  - Configuration changes

**Deliverable:** Working plugin system with Last.fm plugin implementation that can be toggled via configuration without breaking existing functionality.

## Phase 5: Enhanced Management and User Experience

**Goal:** Improve plugin management and user experience.

### 5.1: Enhanced CLI Management

- Complete remaining CLI commands:
  - `navidrome plugin install [file]`
  - `navidrome plugin remove [name]`
  - `navidrome plugin config-template [name]`
- Add command validation and error handling

### 5.2: Plugin Package Format

- Implement `.ndp` package format:
  - Package creation
  - Validation
  - Installation
- Add tests for package integrity checking

### 5.3: Runtime Monitoring

- Add runtime statistics:
  - Plugin execution time
  - Resource usage
  - Error tracking
- Implement health checks and recovery mechanisms

### 5.4: Administrative UI (Optional)

- Create basic admin UI for plugin management:
  - View installed plugins
  - Enable/disable plugins
  - View permissions
  - Configure plugins

**Deliverable:** Complete plugin management tooling with good user experience.

## Phase 6: Documentation and Release

**Goal:** Prepare the plugin system for production use and developer adoption.

### 6.1: User Documentation

- Create comprehensive user documentation:
  - Plugin installation and management
  - Configuration options
  - Security considerations
  - Troubleshooting

### 6.2: Developer Documentation

- Create plugin development guide:
  - API reference
  - Development workflow
  - Best practices
  - Examples

### 6.3: Example Plugin Templates

- Create starter templates for common plugin types:
  - Basic agent plugin
  - Custom service plugin
- Include CI/CD configurations
- Add examples for different permission scenarios:
  - Standard external API access
  - Local network access (with `allowLocalNetwork: true`)
  - Different capability declarations

### 6.4: Final Testing and Feature Flags

- Add feature flag to enable/disable plugin system
- Perform comprehensive integration testing
- Address any final security concerns

**Deliverable:** Production-ready plugin system with documentation and examples.

## Risk Assessment and Mitigation

1. **Security Risks**

   - **Risk**: Plugin execution could compromise system security
   - **Mitigation**: Strict permission model, WebAssembly sandbox, URL validation

2. **Performance Impact**

   - **Risk**: WebAssembly execution might be slower than native code
   - **Mitigation**: Benchmarking, caching mechanisms, performance monitoring

3. **Backward Compatibility**

   - **Risk**: Changes might break existing functionality
   - **Mitigation**: Feature flags, phased integration, comprehensive testing

4. **User Experience**

   - **Risk**: Plugin management could be complex for users
   - **Mitigation**: Clear documentation, intuitive CLI, potential UI integration

5. **Developer Adoption**
   - **Risk**: Plugin development might be too complex
   - **Mitigation**: Clear documentation, example templates, developer tooling
