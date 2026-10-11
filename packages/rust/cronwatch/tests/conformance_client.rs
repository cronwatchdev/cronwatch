//! `conformance/client.json` over the memory store; `cronwatch-sqlx`
//! replays `unknownFields` over its SQL stores too.

mod client_fixture;

use std::sync::Arc;

use cronwatch::MemoryStore;

#[tokio::test]
async fn run_ids_are_1_to_200_characters_wherever_one_is_taken() {
    let cases = client_fixture::replay_run_ids().await;
    assert!(cases >= 36, "{cases} cases");
}

#[tokio::test]
async fn stored_fields_this_release_does_not_know_are_kept_in_memory() {
    assert_eq!(client_fixture::replay_unknown_fields(Arc::new(MemoryStore::new())).await, 5);
}

#[tokio::test]
async fn an_alert_is_retried_to_the_channels_that_refused_it_from_a_trimmed_copy() {
    assert!(client_fixture::replay_outbox().await >= 40);
}
