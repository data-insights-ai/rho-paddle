// Package paddle adapts Paddle Billing to the provider-independent Data Insights
// billing libraries. Hosts own authorization, durable workers and tenant mapping.
// Constructors do not start workers or contact Paddle.
package paddle
