declare module 'autocannon' {
  const autocannon: (options: Record<string, unknown>) => Promise<Record<string, any>>
  export default autocannon
}
