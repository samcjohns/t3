# System Architecture

## 1. Overall Structure

- *Market System*: The actual stock market exchange, which is abstracted away to a public API
- *Public API*: The interface through which clients interact with the market system
- *Web App*: A web-based client that uses the public API of the market system
- *CLI*: A command-line interface that uses the public API of the market system
- *Third-party Integrations*: External systems that interact with the market system through the public API

## 2. Container Structure

### API Gateway

- Responsibilities:
    - authentication
    - authorization
    - routing
    - rate limiting
    - request validation
    - logging
    - metrics
    - translation between external API contracts and internal domain models
- Situated between clients and backend services
- With this structure, no requests hit any other container without being validated first

### Market Engine

- Responsibilities:
    - core matching engine
    - order book management
    - tick orchestration
    - execution reporting
- The "heart" of the system, where all business logic resides
- This does not affect prices, it only matches orders and reports executions

### Ledger

- Responsibilities:
    - maintains account balances
    - maintains stock holdings
    - ingests execution reports from the market engine

### Reporting Service

- Responsibilities:
    - generates reports for clients
    - provides historical data
    - provides analytics and insights
- This service is read-only and does not affect the state of the system
- The Market Engine should not have any analytical functionality, so that would be handled by this service instead
- Can aggregate data from multiple sources within the system
- Caches data for performance and scalability
- Prevents reports and analytics from affecting the performance of the Market Engine
