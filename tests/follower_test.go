package tests

import "time"

func subTestFollower(g *testGroup) {
	g.regSubTest("follow", follower_follow_test)
}

func follower_follow_test(mc *mockServer) error {
	mc2, err := mockOpenServer(MockServerOptions{
		Silent: true, Metrics: false,
	})
	if err != nil {
		return err
	}
	defer mc2.Close()
	err = mc.DoBatch(
		Do("SET", "mykey", "truck1", "POINT", 10, 10).OK(),
		Do("SET", "mykey", "truck2", "POINT", 10, 10).OK(),
		Do("SET", "mykey", "truck3", "POINT", 10, 10).OK(),
		Do("SET", "mykey", "truck4", "POINT", 10, 10).OK(),
		Do("SET", "mykey", "truck5", "POINT", 10, 10).OK(),
		Do("SET", "mykey", "truck6", "POINT", 10, 10).OK(),
	)
	if err != nil {
		return err
	}
	err = mc2.DoBatch(
		Do("SET", "mykey2", "truck1", "POINT", 10, 10).OK(),
		Do("SET", "mykey2", "truck2", "POINT", 10, 10).OK(),
		Do("GET", "mykey2", "truck1").Str(`{"type":"Point","coordinates":[10,10]}`),
		Do("GET", "mykey2", "truck2").Str(`{"type":"Point","coordinates":[10,10]}`),

		Do("FOLLOW", "localhost", mc.port).OK(),
		Do("GET", "mykey2", "truck1").Err("catching up to leader"),
		Sleep(time.Second),
		Do("GET", "mykey2", "truck1").Err(`<nil>`),
		Do("GET", "mykey2", "truck2").Err(`<nil>`),
	)
	if err != nil {
		return err
	}

	// err = mc.DoBatch(
	// 	Do("SET", "mykey", "truck7", "POINT", 10, 10).OK(),
	// 	Do("SET", "mykey", "truck8", "POINT", 10, 10).OK(),
	// 	Do("SET", "mykey", "truck9", "POINT", 10, 10).OK(),
	// )
	// if err != nil {
	// 	return err
	// }

	// err = mc2.DoBatch(
	// 	Sleep(time.Second/2),
	// 	Do("GET", "mykey1", "truck7").Str(`{"type":"Point","coordinates":[10,10]}`),
	// 	Do("GET", "mykey1", "truck8").Str(`{"type":"Point","coordinates":[10,10]}`),
	// 	Do("GET", "mykey1", "truck9").Str(`{"type":"Point","coordinates":[10,10]}`),
	// )
	// if err != nil {
	// 	return err
	// }

	return nil
}
